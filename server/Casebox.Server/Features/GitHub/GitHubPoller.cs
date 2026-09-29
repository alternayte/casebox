using System.Text.Json;
using Casebox.Server.Features.CodeHosts;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.GitHub;

public static class GitHubMessages
{
    public const string PullRequest = "github.pull_request";
    public const string Issue = "github.issue";
    public const string Revert = "github.revert_commit";
}

// Polls every GitHub repository of every workspace. Each changed object is tokenized in memory
// and published to the QueueBox poll source; its key holds the object's updated time, so an
// unchanged object is stored once.
public sealed class GitHubPoller(
    IServiceScopeFactory scopes,
    IntegrationStore integrations,
    NpgsqlDataSource db,
    TimeProvider clock
) : ICodeHost
{
    public string Kind => CodeHostKinds.GitHub;

    public async Task<PollResult> PollAsync(string orgId, CancellationToken ct)
    {
        await using var scope = Scope(orgId);
        var services = scope.ServiceProvider;
        var client = await services.GetRequiredService<GitHubClients>().ForOrgAsync(orgId, ct);
        if (client is null)
            return new PollResult(0, 0, 0, null);
        var settings = (
            await integrations.GetAsync<GitHubSettings, GitHubSecret>(orgId, "github", ct)
        )!
            .Value
            .Config;
        var (org, _) = await services
            .GetRequiredService<IEventStore>()
            .Load<Organisation>(Organisation.StreamId);
        var reader = services.GetRequiredService<GitHubReader>();
        var publisher = services.GetRequiredService<PollPublisher>();

        var result = new PollResult(0, 0, 0, null);
        try
        {
            foreach (var (repo, _) in await CodeHosts.RepoHosts.ReposAsync(db, orgId, Kind, ct))
                result = result.Add(
                    await PollRepoAsync(
                        orgId,
                        repo,
                        client,
                        reader,
                        publisher,
                        org.Settings,
                        settings.Issues,
                        ct
                    )
                );
        }
        catch (GitHubRateLimitedException e)
        {
            result = result with { Error = e.Message };
        }

        await integrations.RecordPollAsync(
            orgId,
            "github",
            new
            {
                at = clock.GetUtcNow(),
                result.PullRequests,
                result.Issues,
                result.Reverts,
                result.Error,
            },
            ct
        );
        return result;
    }

    // Fetches one pull request now, for a webhook.
    public async Task RefreshPullAsync(string orgId, string repo, int number, CancellationToken ct)
    {
        await using var scope = Scope(orgId);
        var services = scope.ServiceProvider;
        var client =
            await services.GetRequiredService<GitHubClients>().ForOrgAsync(orgId, ct)
            ?? throw new InvalidOperationException("GitHub is not connected.");
        var (org, _) = await services
            .GetRequiredService<IEventStore>()
            .Load<Organisation>(Organisation.StreamId);
        var snapshot = await services
            .GetRequiredService<GitHubReader>()
            .PullAsync(client, repo, number, org.Settings, ct);
        await services
            .GetRequiredService<PollPublisher>()
            .PublishAsync(
                new PollMessage(
                    $"github:pr:{repo}#{number}@{snapshot.UpdatedAt:O}",
                    GitHubMessages.PullRequest,
                    orgId,
                    snapshot
                ),
                ct
            );
    }

    // Fetches one issue now, for a webhook.
    public async Task RefreshIssueAsync(string orgId, string repo, int number, CancellationToken ct)
    {
        await using var scope = Scope(orgId);
        var services = scope.ServiceProvider;
        var client =
            await services.GetRequiredService<GitHubClients>().ForOrgAsync(orgId, ct)
            ?? throw new InvalidOperationException("GitHub is not connected.");
        var (org, _) = await services
            .GetRequiredService<IEventStore>()
            .Load<Organisation>(Organisation.StreamId);
        var (owner, name) = GitHubReader.Split(repo);
        var issue = await client.GetAsync($"repos/{owner}/{name}/issues/{number}", ct);
        if (issue.TryGetProperty("pull_request", out _))
            return;
        var snapshot = await services
            .GetRequiredService<GitHubReader>()
            .IssueAsync(issue, repo, org.Settings, ct);
        await services
            .GetRequiredService<PollPublisher>()
            .PublishAsync(
                new PollMessage(
                    $"github:issue:{repo}#{number}@{snapshot.UpdatedAt:O}",
                    GitHubMessages.Issue,
                    orgId,
                    snapshot
                ),
                ct
            );
    }

    private async Task<PollResult> PollRepoAsync(
        string orgId,
        string repo,
        GitHubClient client,
        GitHubReader reader,
        PollPublisher publisher,
        OrgSettings settings,
        bool issues,
        CancellationToken ct
    )
    {
        var (owner, name) = GitHubReader.Split(repo);
        var since = clock.GetUtcNow() - CodeHostPoller.FirstWindow;
        int pulls = 0,
            issueCount = 0,
            reverts = 0;

        var pullCursor = await CursorAsync(orgId, $"github:{repo}:pulls", since, ct);
        var latest = pullCursor;
        var changed = await client.ListAsync(
            $"repos/{owner}/{name}/pulls?state=all&sort=updated&direction=desc",
            pr => pr.GetProperty("updated_at").GetDateTimeOffset() <= pullCursor,
            20,
            ct
        );
        foreach (var pr in changed.OrderBy(p => p.GetProperty("updated_at").GetDateTimeOffset()))
        {
            var snapshot = await reader.PullAsync(
                client,
                repo,
                pr.GetProperty("number").GetInt32(),
                settings,
                ct
            );
            await publisher.PublishAsync(
                new PollMessage(
                    $"github:pr:{repo}#{snapshot.Number}@{snapshot.UpdatedAt:O}",
                    GitHubMessages.PullRequest,
                    orgId,
                    snapshot
                ),
                ct
            );
            latest = Max(latest, snapshot.UpdatedAt);
            await integrations.SetCursorAsync(
                orgId,
                $"github:{repo}:pulls",
                latest.ToString("O"),
                ct
            );
            pulls++;
        }

        if (issues)
        {
            var issueCursor = await CursorAsync(orgId, $"github:{repo}:issues", since, ct);
            var latestIssue = issueCursor;
            foreach (
                var issue in await client.ListAsync(
                    $"repos/{owner}/{name}/issues?state=all&sort=updated&direction=asc&since={issueCursor:O}",
                    null,
                    20,
                    ct
                )
            )
            {
                if (issue.TryGetProperty("pull_request", out _))
                    continue;
                var snapshot = await reader.IssueAsync(issue, repo, settings, ct);
                if (snapshot.UpdatedAt <= issueCursor)
                    continue;
                await publisher.PublishAsync(
                    new PollMessage(
                        $"github:issue:{repo}#{snapshot.Number}@{snapshot.UpdatedAt:O}",
                        GitHubMessages.Issue,
                        orgId,
                        snapshot
                    ),
                    ct
                );
                latestIssue = Max(latestIssue, snapshot.UpdatedAt);
                issueCount++;
            }

            await integrations.SetCursorAsync(
                orgId,
                $"github:{repo}:issues",
                latestIssue.ToString("O"),
                ct
            );
        }

        var repoInfo = await client.GetAsync($"repos/{owner}/{name}", ct);
        var branch = repoInfo.GetProperty("default_branch").GetString();
        var commitCursor = await CursorAsync(orgId, $"github:{repo}:commits", since, ct);
        var latestCommit = commitCursor;
        foreach (
            var commit in await client.ListAsync(
                $"repos/{owner}/{name}/commits?sha={branch}&since={commitCursor:O}",
                null,
                10,
                ct
            )
        )
        {
            var at = commit
                .GetProperty("commit")
                .GetProperty("committer")
                .GetProperty("date")
                .GetDateTimeOffset();
            latestCommit = Max(latestCommit, at);
            if (await reader.RevertAsync(client, repo, commit, settings, ct) is not { } revert)
                continue;
            await publisher.PublishAsync(
                new PollMessage(
                    $"github:revert:{repo}@{revert.Sha}",
                    GitHubMessages.Revert,
                    orgId,
                    revert
                ),
                ct
            );
            reverts++;
        }

        await integrations.SetCursorAsync(
            orgId,
            $"github:{repo}:commits",
            latestCommit.ToString("O"),
            ct
        );
        return new PollResult(pulls, issueCount, reverts, null);
    }

    private async Task<DateTimeOffset> CursorAsync(
        string orgId,
        string source,
        DateTimeOffset fallback,
        CancellationToken ct
    ) =>
        await integrations.CursorAsync(orgId, source, ct) is { } c
            ? DateTimeOffset.Parse(c, System.Globalization.CultureInfo.InvariantCulture)
            : fallback;

    private AsyncServiceScope Scope(string orgId)
    {
        var scope = scopes.CreateAsyncScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = orgId;
        context.Metadata = new EventMetadata { Actor = "system:github" };
        return scope;
    }

    private static DateTimeOffset Max(DateTimeOffset a, DateTimeOffset b) => a > b ? a : b;
}
