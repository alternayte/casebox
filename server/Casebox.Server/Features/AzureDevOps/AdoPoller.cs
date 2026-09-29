using System.Globalization;
using System.Text.Json;
using Casebox.Server.Features.CodeHosts;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.AzureDevOps;

public static class AdoMessages
{
    public const string PullRequest = "ado.pull_request";
    public const string Revert = "ado.revert_commit";
}

// Polls the Azure DevOps Server repositories of every workspace (docs/specs/azure-devops.md):
// the active pull requests, those closed since the last poll, and the commits of the default
// branch. Each changed object is tokenized in memory and published to the QueueBox poll source,
// in the shape GitHub polling publishes, so one set of handlers applies both.
public sealed class AdoPoller(
    IServiceScopeFactory scopes,
    IntegrationStore integrations,
    AdoClients clients,
    NpgsqlDataSource db,
    TimeProvider clock
) : ICodeHost
{
    // Pull requests created this long before the last poll are not looked for among closed ones.
    private static readonly TimeSpan ClosedLookback = TimeSpan.FromDays(90);

    public string Kind => CodeHostKinds.AzureDevOps;

    public async Task<PollResult> PollAsync(string orgId, CancellationToken ct)
    {
        var byCollection = await clients.ForOrgAsync(orgId, ct);
        if (byCollection.Count == 0)
            return new PollResult(0, 0, 0, null);
        await using var scope = scopes.CreateAsyncScope();
        var services = scope.ServiceProvider;
        var context = services.GetRequiredService<DeedboxContext>();
        context.TenantId = orgId;
        context.Metadata = new EventMetadata { Actor = "system:azure-devops" };
        var (org, _) = await services
            .GetRequiredService<IEventStore>()
            .Load<Organisation>(Organisation.StreamId);
        var reader = services.GetRequiredService<AdoReader>();
        var publisher = services.GetRequiredService<PollPublisher>();

        var result = new PollResult(0, 0, 0, null);
        var people = byCollection.ToDictionary(c => c.Key, c => new AdoPeople(c.Value));
        foreach (
            var (repo, collection) in await CodeHosts.RepoHosts.ReposAsync(db, orgId, Kind, ct)
        )
        {
            if (collection is null || !byCollection.TryGetValue(collection, out var client))
                continue;
            try
            {
                result = result.Add(
                    await PollRepoAsync(
                        orgId,
                        Repo(repo, collection),
                        client,
                        people[collection],
                        reader,
                        publisher,
                        org.Settings,
                        ct
                    )
                );
            }
            catch (HttpRequestException e)
            {
                result = result with { Error = $"{repo}: {e.Message}" };
            }
        }

        await integrations.RecordPollAsync(
            orgId,
            Kind,
            new
            {
                at = clock.GetUtcNow(),
                result.PullRequests,
                result.Reverts,
                result.Error,
                identitiesRefused = people.Values.Any(p => p.Refused),
            },
            ct
        );
        return result;
    }

    public static AdoRepo Repo(string repo, string collection)
    {
        var (project, name) = CodeHosts.RepoHosts.AdoPath(repo, collection);
        return new AdoRepo(repo, project, name);
    }

    private async Task<PollResult> PollRepoAsync(
        string orgId,
        AdoRepo repo,
        AdoClient client,
        AdoPeople people,
        AdoReader reader,
        PollPublisher publisher,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var since = clock.GetUtcNow() - CodeHostPoller.FirstWindow;
        var info = await client.GetAsync(repo.Api, ct);
        var projectId = info.TryGetProperty("project", out var p) ? AdoPeople.Text(p, "id") : null;

        // The default branch first: its "Merged PR <n>:" commits are the merge commits of the
        // pull requests read below, and its reverts are after-merge corrections. The reverts are
        // published after the pull requests, whose rows their handler reads.
        var mergeCommits = new Dictionary<int, string>();
        var reverts = new List<RevertCommit>();
        var commitCursorSource = $"ado:{repo.Name}:commits";
        var latestCommit = DateTimeOffset.MinValue;
        if (AdoPeople.Text(info, "defaultBranch") is { } defaultBranch)
        {
            var branch = defaultBranch.StartsWith("refs/heads/", StringComparison.Ordinal)
                ? defaultBranch["refs/heads/".Length..]
                : defaultBranch;
            var commitCursor = await CursorAsync(orgId, commitCursorSource, since, ct);
            latestCommit = commitCursor;
            var commits = await client.ListAsync(
                $"{repo.Api}/commits?searchCriteria.itemVersion.version={Uri.EscapeDataString(branch)}&searchCriteria.fromDate={Uri.EscapeDataString(commitCursor.UtcDateTime.ToString("yyyy-MM-ddTHH:mm:ssZ", CultureInfo.InvariantCulture))}",
                20,
                ct,
                prefix: "searchCriteria."
            );
            foreach (var commit in commits)
            {
                var sha = commit.GetProperty("commitId").GetString()!;
                if (AdoReader.MergedPrOf(AdoPeople.Text(commit, "comment")) is { } merged)
                    mergeCommits.TryAdd(merged, sha);
            }

            foreach (
                var commit in commits.OrderBy(c =>
                    AdoReader.Date(c.GetProperty("committer"), "date")
                )
            )
            {
                if (AdoReader.Date(commit.GetProperty("committer"), "date") is { } at)
                    latestCommit = Max(latestCommit, at);
                if (await reader.RevertAsync(client, repo, commit, settings, ct) is { } revert)
                    reverts.Add(revert);
            }
        }

        // Active pull requests are read on every poll: Azure DevOps has no "updated since" query
        // in API 7.0. The snapshot's updated time is the latest of its iterations, threads, statuses
        // and policy runs, so QueueBox stores an unchanged pull request once.
        var closedCursor = await CursorAsync(orgId, $"ado:{repo.Name}:closed", since, ct);
        var latestClosed = closedCursor;
        var pulls = await client.ListAsync(
            $"{repo.Api}/pullrequests?searchCriteria.status=active",
            10,
            ct
        );
        pulls.AddRange(
            (
                await client.ListAsync(
                    $"{repo.Api}/pullrequests?searchCriteria.status=all",
                    20,
                    ct,
                    pr =>
                        AdoReader.Date(pr, "creationDate") is { } created
                        && created < closedCursor - ClosedLookback
                )
            ).Where(pr =>
                AdoPeople.Text(pr, "status") is "completed" or "abandoned"
                && AdoReader.Date(pr, "closedDate") is { } closed
                && closed > closedCursor
            )
        );

        var count = 0;
        foreach (
            var pr in pulls
                .DistinctBy(pr => pr.GetProperty("pullRequestId").GetInt32())
                .OrderBy(pr => AdoReader.Date(pr, "closedDate") ?? DateTimeOffset.MaxValue)
        )
        {
            var snapshot = await reader.PullAsync(
                client,
                people,
                repo,
                pr,
                projectId,
                mergeCommits,
                settings,
                ct
            );
            await publisher.PublishAsync(
                new PollMessage(
                    $"ado:pr:{repo.Name}#{snapshot.Number}@{snapshot.UpdatedAt:O}",
                    AdoMessages.PullRequest,
                    orgId,
                    snapshot
                ),
                ct
            );
            if (AdoReader.Date(pr, "closedDate") is { } closed && snapshot.State == "closed")
            {
                latestClosed = Max(latestClosed, closed);
                await integrations.SetCursorAsync(
                    orgId,
                    $"ado:{repo.Name}:closed",
                    latestClosed.ToString("O"),
                    ct
                );
            }

            count++;
        }

        foreach (var revert in reverts)
            await publisher.PublishAsync(
                new PollMessage(
                    $"ado:revert:{repo.Name}@{revert.Sha}",
                    AdoMessages.Revert,
                    orgId,
                    revert
                ),
                ct
            );
        if (latestCommit != DateTimeOffset.MinValue)
            await integrations.SetCursorAsync(
                orgId,
                commitCursorSource,
                latestCommit.ToString("O"),
                ct
            );
        return new PollResult(count, 0, reverts.Count, null);
    }

    private async Task<DateTimeOffset> CursorAsync(
        string orgId,
        string source,
        DateTimeOffset fallback,
        CancellationToken ct
    ) =>
        await integrations.CursorAsync(orgId, source, ct) is { } c
            ? DateTimeOffset.Parse(c, CultureInfo.InvariantCulture)
            : fallback;

    private static DateTimeOffset Max(DateTimeOffset a, DateTimeOffset b) => a > b ? a : b;
}

// Who is who on Azure DevOps Server: the authors and reviewers of each repository's recent pull
// requests, with the mail and display name _apis/identities gives, which join them to their git
// email and name. Build services and identities without mail are left out. Read into memory for
// the roster; never stored.
public sealed class AdoRosterSource(AdoClients clients, NpgsqlDataSource db) : IRosterSource
{
    private const int PullPages = 3;

    public async Task<IReadOnlyList<RosterPerson>> PeopleAsync(string orgId, CancellationToken ct)
    {
        var byCollection = await clients.ForOrgAsync(orgId, ct);
        if (byCollection.Count == 0)
            return [];
        var ids = new Dictionary<string, HashSet<string>>(StringComparer.Ordinal);
        foreach (
            var (repo, collection) in await CodeHosts.RepoHosts.ReposAsync(
                db,
                orgId,
                CodeHostKinds.AzureDevOps,
                ct
            )
        )
        {
            if (collection is null || !byCollection.TryGetValue(collection, out var client))
                continue;
            var set = ids.TryGetValue(collection, out var s)
                ? s
                : ids[collection] = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
            foreach (
                var pr in await client.ListAsync(
                    $"{AdoPoller.Repo(repo, collection).Api}/pullrequests?searchCriteria.status=all",
                    PullPages,
                    ct
                )
            )
            {
                var refs = new List<JsonElement> { pr.GetProperty("createdBy") };
                if (pr.TryGetProperty("reviewers", out var reviewers))
                    refs.AddRange(reviewers.EnumerateArray());
                foreach (var r in refs)
                    if (!AdoPeople.IsService(r) && AdoPeople.Text(r, "id") is { } id)
                        set.Add(id);
            }
        }

        var people = new List<RosterPerson>();
        foreach (var (collection, set) in ids)
        {
            var lookup = new AdoPeople(byCollection[collection]);
            await lookup.LoadAsync(set, ct);
            foreach (var id in set)
            {
                if (await lookup.GetAsync(id, ct) is not { Group: false, Mail: { } mail } identity)
                    continue;
                var linked = new List<string> { $"email:{mail}" };
                if (identity.DisplayName is { Length: > 2 } name)
                    linked.Add($"name:{name}");
                people.Add(new RosterPerson($"ado:{id}", linked));
            }
        }

        return people;
    }
}
