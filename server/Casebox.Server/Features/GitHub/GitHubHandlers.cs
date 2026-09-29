using System.Data.Common;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.WorkItems;
using Dapper;
using Deedbox;
using QueueBox.Inbox;

namespace Casebox.Server.Features.GitHub;

// Applies tokenized GitHub poll results inside the transaction QueueBox.Inbox opened, so the
// inbox completion and every event commit together or not at all.
public sealed class PullRequestHandler(Linker linker, Jobs.JobQueue jobs) : IInboxHandler
{
    public static readonly TimeSpan FixWindow = TimeSpan.FromDays(30);

    // A proposal's pull request (docs/specs/self-evolution.md, After the merge): merged records the
    // merge; closed without a merge records a rejection, unless a person gave a reason first.
    private static async Task ProposalPullAsync(
        DbConnection connection,
        DbTransaction transaction,
        IEventStore store,
        string org,
        PullRequestSnapshot pr,
        CancellationToken ct
    )
    {
        const string prefix = "casebox/proposal-";
        if (pr.HeadRef?.StartsWith(prefix, StringComparison.Ordinal) != true)
            return;
        var id = await connection.ExecuteScalarAsync<string?>(
            new CommandDefinition(
                "SELECT id FROM casebox.proposals WHERE org_id = @Org AND lower(id) = @Id AND status = 'pr_opened'",
                new { Org = org, Id = pr.HeadRef[prefix.Length..] },
                transaction,
                cancellationToken: ct
            )
        );
        if (id is null)
            return;
        if (pr.MergedAt is { } merged)
            await store.Execute<Proposals.Proposal>(
                Proposals.Proposal.StreamId(id),
                p => Proposals.ProposalDecider.Merge(p, merged, pr.MergeSha),
                ct
            );
        else if (pr.State == "closed")
            await store.Execute<Proposals.Proposal>(
                Proposals.Proposal.StreamId(id),
                p => Proposals.ProposalDecider.Reject(p, "closed without merge", "system:github"),
                ct
            );
    }

    public string Source => InboxSources.Poll;

    public string EventType => GitHubMessages.PullRequest;

    public async Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        var pr = message
            .Payload.GetProperty("payload")
            .Deserialize<PullRequestSnapshot>(GitHubJson.Options)!;
        var connection = transaction.Connection!;
        var org = message.Payload.GetProperty("org").GetString()!;
        var repo = pr.Repo.ToLowerInvariant();
        var shas = pr.Commits.Select(c => c.Sha).ToArray();

        var isAgent =
            pr.Commits.Any(c => c.AgentCoAuthor)
            || await connection.ExecuteScalarAsync<bool>(
                new CommandDefinition(
                    """
                    SELECT EXISTS (SELECT 1 FROM casebox.sessions WHERE org_id = @Org AND repo = @Repo AND branch = @Branch)
                        OR EXISTS (SELECT 1 FROM casebox.commit_attributions WHERE org_id = @Org AND repo = @Repo AND sha = ANY(@Shas))
                    """,
                    new
                    {
                        Org = org,
                        Repo = repo,
                        Branch = pr.HeadRef,
                        Shas = shas,
                    },
                    transaction,
                    cancellationToken: ct
                )
            );

        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.pull_requests (org_id, repo, number, state, head_ref, base_ref, author, is_agent, merged_at, merge_sha, updated_at, snapshot)
                VALUES (@Org, @Repo, @Number, @State, @Head, @Base, @Author, @Agent, @MergedAt, @MergeSha, @Updated, @Snapshot::jsonb)
                ON CONFLICT (org_id, repo, number) DO UPDATE SET state = EXCLUDED.state, head_ref = EXCLUDED.head_ref, base_ref = EXCLUDED.base_ref,
                    is_agent = pull_requests.is_agent OR EXCLUDED.is_agent, merged_at = EXCLUDED.merged_at, merge_sha = EXCLUDED.merge_sha,
                    updated_at = EXCLUDED.updated_at, snapshot = EXCLUDED.snapshot, changed_at = now()
                WHERE pull_requests.updated_at <= EXCLUDED.updated_at;
                INSERT INTO casebox.pr_checks (org_id, repo, number, sha, name, conclusion, completed_at)
                SELECT @Org, @Repo, @Number, c->>'headSha', c->>'name', c->>'conclusion', (c->>'completedAt')::timestamptz
                FROM jsonb_array_elements(@Snapshot::jsonb->'checks') c
                WHERE c->>'conclusion' IS NOT NULL AND c->>'completedAt' IS NOT NULL
                ON CONFLICT DO NOTHING;
                """,
                new
                {
                    Org = org,
                    Repo = repo,
                    pr.Number,
                    State = pr.MergedAt is null ? pr.State : "merged",
                    Head = pr.HeadRef,
                    Base = pr.BaseRef,
                    Author = pr.Author?.Token ?? "unknown",
                    Agent = isAgent,
                    pr.MergedAt,
                    pr.MergeSha,
                    Updated = pr.UpdatedAt,
                    Snapshot = JsonSerializer.Serialize(
                        pr with
                        {
                            Files = pr.Files,
                        },
                        GitHubJson.Options
                    ),
                },
                transaction,
                cancellationToken: ct
            )
        );

        await ProposalPullAsync(connection, transaction, store, org, pr, ct);

        var keys = await linker.KeysAsync(ct);
        var named = keys.FromPullRequest(pr.Title, pr.Body, repo)
            .Select(w => (Id: w, Source: LinkSource.PullRequest))
            .Concat(
                keys.FromBranch(pr.HeadRef, repo).Select(w => (Id: w, Source: LinkSource.Branch))
            )
            .DistinctBy(w => w.Id)
            .ToList();

        var linked = new List<string>();
        foreach (var (id, source) in named)
        {
            var (item, _) = await store.Load<WorkItem>(id);
            if (!item.Exists)
                continue;
            var current = await linker.CurrentSnapshotAsync(id, ct);
            await store.Execute<WorkItem>(
                id,
                w => WorkItemDecider.LinkPr(w, repo, pr.Number, source, Confidence.High, current)
            );
            linked.Add(id);
        }

        foreach (var id in linked)
        {
            foreach (var review in pr.Reviews.Where(r => r.At is not null && r.State != "PENDING"))
            {
                var comments = pr.ReviewComments.Count(c => c.ReviewId == review.Id);
                await store.Execute<WorkItem>(
                    id,
                    w =>
                        WorkItemDecider.ObserveReview(
                            w,
                            new WorkItemEvents.ReviewObserved(
                                repo,
                                pr.Number,
                                review.Id,
                                review.State,
                                review.Author?.Token ?? "unknown",
                                comments,
                                review.At!.Value
                            )
                        )
                );
            }

            foreach (
                var check in pr.Checks.Where(c =>
                    c.Conclusion is not null && c.CompletedAt is not null
                )
            )
                await store.Execute<WorkItem>(
                    id,
                    w =>
                        WorkItemDecider.ObserveCi(
                            w,
                            new WorkItemEvents.CiObserved(
                                repo,
                                pr.Number,
                                check.HeadSha,
                                check.Name,
                                check.Conclusion!,
                                check.CompletedAt!.Value
                            )
                        )
                );

            if (pr.MergedAt is { } mergedAt)
                await store.Execute<WorkItem>(
                    id,
                    w =>
                        WorkItemDecider.Merge(
                            w,
                            new WorkItemEvents.Merged(
                                repo,
                                pr.Number,
                                pr.MergeSha ?? pr.HeadSha,
                                mergedAt
                            )
                        )
                );
        }

        // Sessions on this branch follow the pull request's work item: "same branch, PR opened later".
        if (linked.FirstOrDefault() is { } primary)
        {
            var current = await linker.CurrentSnapshotAsync(primary, ct);
            var sessions = await connection.QueryAsync<string>(
                new CommandDefinition(
                    "SELECT id FROM casebox.sessions WHERE org_id = @Org AND repo = @Repo AND branch = @Branch AND work_item_id IS NULL",
                    new
                    {
                        Org = org,
                        Repo = repo,
                        Branch = pr.HeadRef,
                    },
                    transaction,
                    cancellationToken: ct
                )
            );
            foreach (var session in sessions)
                await store.Execute<WorkItem>(
                    primary,
                    w =>
                        WorkItemDecider.LinkSession(
                            w,
                            session,
                            LinkSource.SameBranch,
                            Confidence.High,
                            current
                        )
                );
        }

        if (pr.MergedAt is not { } merged)
            return;
        var self = WorkItem.Pr(repo, pr.Number);
        await Repos.RepoJobKinds.EnqueueAfterMergeAsync(
            jobs,
            transaction,
            org,
            repo,
            pr.MergeSha ?? pr.HeadSha,
            ct
        );

        if (pr.Reverts is { } reverted)
            await ObserveOnAsync(
                connection,
                transaction,
                store,
                org,
                repo,
                reverted,
                ct,
                w =>
                    WorkItemDecider.ObserveRevert(
                        w,
                        new WorkItemEvents.RevertObserved(repo, reverted, self, merged)
                    )
            );

        // A fix changes lines an agent pull request wrote, within 30 days of its merge.
        foreach (var blamed in pr.Blamed)
        {
            var target = await connection.QuerySingleOrDefaultAsync<(
                bool IsAgent,
                DateTime? MergedAt
            )?>(
                new CommandDefinition(
                    "SELECT is_agent, merged_at FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number",
                    new
                    {
                        Org = org,
                        Repo = blamed.Repo,
                        blamed.Number,
                    },
                    transaction,
                    cancellationToken: ct
                )
            );
            if (target is not { IsAgent: true, MergedAt: { } targetMerged })
                continue;
            var at = new DateTimeOffset(DateTime.SpecifyKind(targetMerged, DateTimeKind.Utc));
            if (at > merged || merged - at > FixWindow)
                continue;
            await ObserveOnAsync(
                connection,
                transaction,
                store,
                org,
                blamed.Repo,
                blamed.Number,
                ct,
                w =>
                    WorkItemDecider.ObserveFix(
                        w,
                        new WorkItemEvents.FixObserved(
                            blamed.Repo,
                            blamed.Number,
                            self,
                            blamed.Lines,
                            LinkSource.PullRequest,
                            merged
                        )
                    )
            );
        }

        // A later pull request of the same work item fixes its earlier agent pull requests too.
        foreach (var id in linked)
        {
            var earlier = await connection.QueryAsync<int>(
                new CommandDefinition(
                    """
                    SELECT number FROM casebox.pull_requests
                    WHERE org_id = @Org AND repo = @Repo AND work_item_id = @Id AND is_agent AND number <> @Number
                      AND merged_at IS NOT NULL AND merged_at < @Merged AND merged_at > @Since
                    """,
                    new
                    {
                        Org = org,
                        Repo = repo,
                        Id = id,
                        pr.Number,
                        Merged = merged,
                        Since = merged - FixWindow,
                    },
                    transaction,
                    cancellationToken: ct
                )
            );
            foreach (var number in earlier.Where(n => !pr.Blamed.Any(b => b.Number == n)))
                await store.Execute<WorkItem>(
                    id,
                    w =>
                        WorkItemDecider.ObserveFix(
                            w,
                            new WorkItemEvents.FixObserved(
                                repo,
                                number,
                                self,
                                0,
                                LinkSource.SameWorkItem,
                                merged
                            )
                        )
                );
        }
    }

    // Appends to the work item of a merged pull request, when that pull request has one.
    internal static async Task ObserveOnAsync(
        DbConnection connection,
        DbTransaction transaction,
        IEventStore store,
        string org,
        string repo,
        int number,
        CancellationToken ct,
        Func<WorkItem, IEnumerable<object>> decide
    )
    {
        var workItem = await connection.QuerySingleOrDefaultAsync<string>(
            new CommandDefinition(
                "SELECT work_item_id FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number",
                new
                {
                    Org = org,
                    Repo = repo,
                    Number = number,
                },
                transaction,
                cancellationToken: ct
            )
        );
        if (workItem is null)
            return;
        var (item, _) = await store.Load<WorkItem>(workItem);
        if (!item.MergedPullRequests.Contains(WorkItem.Pr(repo, number)))
            return;
        await store.Execute<WorkItem>(workItem, decide);
    }
}

public sealed class IssueHandler(Linker linker) : IInboxHandler
{
    public string Source => InboxSources.Poll;

    public string EventType => GitHubMessages.Issue;

    public async Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        var issue = message
            .Payload.GetProperty("payload")
            .Deserialize<IssueSnapshot>(GitHubJson.Options)!;
        var repo = issue.Repo.ToLowerInvariant();
        var id = WorkItem.GitHubStream(repo, issue.Number);
        var snapshot = WorkItemSnapshots.Of(
            issue.Title ?? "",
            issue.Body,
            "issue",
            issue.Labels,
            issue.State,
            issue.Assignee?.Token,
            issue.ClosedAt is not null
        );
        await store.Execute<WorkItem>(
            id,
            w =>
                WorkItemDecider.Import(
                    w,
                    "github",
                    $"{repo}#{issue.Number}",
                    repo,
                    issue.CreatedAt,
                    snapshot
                )
        );
        await WorkItemSnapshots.LinkWaitingAsync(
            transaction,
            store,
            linker,
            message.Payload.GetProperty("org").GetString()!,
            id,
            repo,
            ct
        );
    }
}

public sealed class RevertCommitHandler : IInboxHandler
{
    public string Source => InboxSources.Poll;

    public string EventType => GitHubMessages.Revert;

    public async Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        var revert = message
            .Payload.GetProperty("payload")
            .Deserialize<RevertCommit>(GitHubJson.Options)!;
        if (revert.RevertedPr is not { } number)
            return;
        var repo = revert.Repo.ToLowerInvariant();
        var org = message.Payload.GetProperty("org").GetString()!;
        await PullRequestHandler.ObserveOnAsync(
            transaction.Connection!,
            transaction,
            store,
            org,
            repo,
            number,
            ct,
            w =>
                WorkItemDecider.ObserveRevert(
                    w,
                    new WorkItemEvents.RevertObserved(repo, number, revert.Sha, revert.At)
                )
        );

        // A revert of an agent pull request is an after-merge correction, with or without a work item.
        var isAgent = await transaction.Connection!.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                "SELECT EXISTS (SELECT 1 FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number AND is_agent AND merged_at IS NOT NULL)",
                new
                {
                    Org = org,
                    Repo = repo,
                    Number = number,
                },
                transaction,
                cancellationToken: ct
            )
        );
        if (!isAgent || revert.Author is not { Bot: false } author)
            return;
        var (settings, _) = await store.Load<Orgs.Organisation>(Orgs.Organisation.StreamId, ct);
        await store.Execute<Steering.SteeringState>(
            Steering.SteeringState.PullRequestStream(repo, number),
            s =>
                Steering.SteeringDecider.Observe(
                    s,
                    [
                        new Steering.SteeringEvents.Observed(
                            $"revert:commit:{revert.Sha}",
                            Steering.Signal.Revert,
                            Steering.Phase.AfterMerge,
                            revert.At,
                            repo,
                            null,
                            number,
                            author.Token,
                            author.Mapped,
                            Capture.Identities.PeriodOf(
                                settings.Settings.PseudonymPeriod,
                                revert.At
                            ),
                            revert.Message,
                            Steering.Intent.Correction,
                            new Steering.SteeringRefs(Commits: [revert.Sha])
                        ),
                    ]
                ),
            ct
        );
    }
}

public static class GitHubJson
{
    public static readonly JsonSerializerOptions Options = new(JsonSerializerDefaults.Web);
}

public static class WorkItemSnapshots
{
    public static Snapshot Of(
        string title,
        string? description,
        string? type,
        IReadOnlyList<string> labels,
        string? status,
        string? assignee,
        bool resolved
    )
    {
        var hash = Convert.ToHexStringLower(
            SHA256.HashData(Encoding.UTF8.GetBytes($"{title}\n{description}"))
        );
        return new Snapshot(title, description, type, labels, status, assignee, resolved, hash);
    }

    // Links the sessions that name a work item which has just been imported: explicitly, or by branch.
    public static async Task LinkWaitingAsync(
        DbTransaction transaction,
        IEventStore store,
        Linker linker,
        string org,
        string workItemId,
        string? repo,
        CancellationToken ct
    )
    {
        var keys = await linker.KeysAsync(ct);
        var sessions = await transaction.Connection!.QueryAsync<(
            string Id,
            string? Repo,
            string? Branch,
            string? WorkItem
        )>(
            new CommandDefinition(
                """
                SELECT id, repo, branch, work_item FROM casebox.sessions
                WHERE org_id = @Org AND work_item_id IS NULL AND (@Repo::text IS NULL OR repo = @Repo)
                  AND (work_item IS NOT NULL OR branch IS NOT NULL) AND started_at > now() - interval '183 days'
                """,
                new { Org = org, Repo = repo },
                transaction,
                cancellationToken: ct
            )
        );
        Snapshot? snapshot = null;
        foreach (var s in sessions)
        {
            var (source, confidence) =
                keys.FromExplicit(s.WorkItem, s.Repo) == workItemId
                    ? (LinkSource.Explicit, Confidence.Explicit)
                : s.Repo is not null && keys.FromBranch(s.Branch, s.Repo).Contains(workItemId)
                    ? (LinkSource.Branch, Confidence.High)
                : ((LinkSource?)null, Confidence.Low);
            if (source is null)
                continue;
            snapshot ??= await linker.CurrentSnapshotAsync(workItemId, ct);
            await store.Execute<WorkItem>(
                workItemId,
                w => WorkItemDecider.LinkSession(w, s.Id, source.Value, confidence, snapshot)
            );
        }
    }
}
