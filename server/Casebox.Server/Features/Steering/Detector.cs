using System.Text.Json;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Steering;

// Runs the steering detection for every organisation once a minute. The work itself is a scoped
// SteeringScan, which the refresh route also runs on demand.
public sealed class SteeringDetector(IServiceScopeFactory scopes, NpgsqlDataSource db, TimeProvider clock, ILogger<SteeringDetector> logger) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await RunAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogError(e, "The steering detection pass failed; it runs again in a minute.");
            }

            await Task.Delay(TimeSpan.FromMinutes(1), clock, stoppingToken);
        }
    }

    public async Task RunAsync(CancellationToken ct)
    {
        IEnumerable<string> orgs;
        await using (var connection = await db.OpenConnectionAsync(ct))
            orgs = (await connection.QueryAsync<string>(new CommandDefinition(
                "SELECT org_id FROM casebox.sessions UNION SELECT org_id FROM casebox.pull_requests", cancellationToken: ct))).ToList();
        foreach (var org in orgs)
        {
            await using var scope = scopes.CreateAsyncScope();
            var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
            context.TenantId = org;
            context.Metadata = new EventMetadata { Actor = "system:steering" };
            await scope.ServiceProvider.GetRequiredService<SteeringScan>().RunAsync(null, ct);
        }
    }
}

// One detection pass for the current organisation (docs/specs/steering.md, Signals): in-session
// interventions of changed sessions, the outcome of ended sessions, the signals of changed pull
// requests, and classification jobs for whatever is still pending. Every step is idempotent: the
// steering decider appends an intervention once.
public sealed class SteeringScan(NpgsqlDataSource db, IEventStore store, DeedboxContext context, JobQueue jobs, TimeProvider clock)
{
    public static readonly TimeSpan EndedAfter = TimeSpan.FromHours(1);
    public static readonly TimeSpan RestartWindow = TimeSpan.FromHours(2);
    public static readonly TimeSpan SessionCommitSlack = TimeSpan.FromMinutes(10);
    private const int Batch = 500;

    private string Org => context.TenantId;

    public async Task RunAsync(string? repo, CancellationToken ct)
    {
        // One pass per organisation at a time: the minute loop and a refresh never interleave.
        await using var lockConnection = await db.OpenConnectionAsync(ct);
        await lockConnection.ExecuteAsync(new CommandDefinition("SELECT pg_advisory_lock(hashtext(@Key))", new { Key = $"steering:{Org}" }, cancellationToken: ct));
        try
        {
            var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
            while (await ScanSessionsAsync(repo, ct) == Batch) { }
            while (await FinishSessionsAsync(repo, ct) == Batch) { }
            while (await ScanPullRequestsAsync(repo, org.Settings, ct) == Batch) { }
            await EnqueuePendingAsync(ct);
        }
        finally
        {
            await lockConnection.ExecuteAsync(new CommandDefinition("SELECT pg_advisory_unlock(hashtext(@Key))", new { Key = $"steering:{Org}" }, cancellationToken: CancellationToken.None));
        }
    }

    private sealed record SessionRow(string Id, string Agent, string? Model, string? Repo, string? Branch, string Person, bool PersonMapped, string Period,
        DateTime StartedAt, DateTime? EndedAt, int? Commits, string? TaskType, DateTime UpdatedAt);

    private const string SessionColumns = "id, agent, model, repo, branch, person, person_mapped, period, started_at, ended_at, commits, task_type, updated_at";

    private async Task<int> ScanSessionsAsync(string? repo, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var sessions = (await connection.QueryAsync<SessionRow>(new CommandDefinition(
            $"""
            SELECT {SessionColumns} FROM casebox.sessions
            WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo) AND period_retired_at IS NULL
              AND (steering_scanned_at IS NULL OR steering_scanned_at < updated_at)
            ORDER BY started_at LIMIT {Batch}
            """,
            new { Org, Repo = repo }, cancellationToken: ct))).ToList();

        foreach (var s in sessions)
        {
            if (s.Repo is not null)
            {
                var events = await connection.QueryAsync<(long Seq, DateTime At, string Kind, string? Text, string? Denial)>(new CommandDefinition(
                    """
                    SELECT seq, at, kind, CASE WHEN kind = 'prompt' THEN text END, attrs->>'denial'
                    FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id AND kind IN ('prompt', 'response', 'tool_call', 'tool_result', 'interruption', 'denial', 'rewind', 'human_edit')
                    ORDER BY at, seq
                    """,
                    new { Org, s.Id }, cancellationToken: ct));
                var found = InSession.Detect(events.Select(e => new TraceEvent(e.Seq, Utc(e.At), e.Kind, e.Text, e.Denial)).ToList());
                if (found.Count > 0)
                    await store.Execute<SteeringState>(SteeringState.SessionStream(s.Id), state => SteeringDecider.Observe(state, found.Select(i =>
                        new SteeringEvents.Observed(i.Id, i.Signal, Phase.InSession, i.At, s.Repo, s.Id, null, s.Person, s.PersonMapped, s.Period, i.Text, null,
                            new SteeringRefs(Seqs: i.Seqs)))), ct);
                await RestartsBeforeAsync(connection, s, ct);
                if (s.TaskType is null) await EnqueueTaskAsync(s.Id, ct);
            }

            await connection.ExecuteAsync(new CommandDefinition(
                "UPDATE casebox.sessions SET steering_scanned_at = @Seen WHERE org_id = @Org AND id = @Id AND (steering_scanned_at IS NULL OR steering_scanned_at < @Seen)",
                new { Org, s.Id, Seen = s.UpdatedAt }, cancellationToken: ct));
        }

        return sessions.Count;
    }

    // An ended session with agent activity and no commit is abandoned; one that the same person
    // followed on the same branch with another agent or model within two hours was restarted.
    private async Task<int> FinishSessionsAsync(string? repo, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var sessions = (await connection.QueryAsync<SessionRow>(new CommandDefinition(
            $"""
            SELECT {SessionColumns} FROM casebox.sessions
            WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo) AND period_retired_at IS NULL AND steering_final_at IS NULL
              AND ended_at < @Ended
            ORDER BY started_at LIMIT {Batch}
            """,
            new { Org, Repo = repo, Ended = clock.GetUtcNow() - EndedAfter }, cancellationToken: ct))).ToList();

        foreach (var s in sessions)
        {
            if (s.Repo is not null && s.Commits == 0 && await ActiveAsync(connection, s.Id, ct))
            {
                await store.Execute<SteeringState>(SteeringState.SessionStream(s.Id), state => SteeringDecider.Observe(state,
                    [new SteeringEvents.Observed("abandoned", Signal.Abandoned, Phase.InSession, Utc(s.EndedAt!.Value), s.Repo, s.Id, null, s.Person, s.PersonMapped, s.Period, null, null, new SteeringRefs())]), ct);
                var later = await connection.QueryAsync<SessionRow>(new CommandDefinition(
                    $"""
                    SELECT {SessionColumns} FROM casebox.sessions
                    WHERE org_id = @Org AND repo = @Repo AND branch = @Branch AND person = @Person AND id <> @Id
                      AND started_at >= @Ended AND started_at < @Until AND (agent <> @Agent OR coalesce(model, '') <> coalesce(@Model, ''))
                    ORDER BY started_at LIMIT 1
                    """,
                    new { Org, s.Repo, s.Branch, s.Person, s.Id, Ended = s.EndedAt, Until = s.EndedAt + RestartWindow, s.Agent, s.Model }, cancellationToken: ct));
                foreach (var next in later) await RestartedAsync(connection, s, next, ct);
            }

            await connection.ExecuteAsync(new CommandDefinition(
                "UPDATE casebox.sessions SET steering_final_at = @Now WHERE org_id = @Org AND id = @Id",
                new { Org, s.Id, Now = clock.GetUtcNow() }, cancellationToken: ct));
        }

        return sessions.Count;
    }

    // A new session may restart one that ended before it arrived.
    private async Task RestartsBeforeAsync(NpgsqlConnection connection, SessionRow next, CancellationToken ct)
    {
        if (next.Branch is null) return;
        var earlier = await connection.QueryAsync<SessionRow>(new CommandDefinition(
            $"""
            SELECT {SessionColumns} FROM casebox.sessions
            WHERE org_id = @Org AND repo = @Repo AND branch = @Branch AND person = @Person AND id <> @Id AND commits = 0
              AND ended_at <= @Started AND ended_at > @Since AND (agent <> @Agent OR coalesce(model, '') <> coalesce(@Model, ''))
            """,
            new { Org, next.Repo, next.Branch, next.Person, next.Id, Started = next.StartedAt, Since = next.StartedAt - RestartWindow, next.Agent, next.Model }, cancellationToken: ct));
        foreach (var s in earlier)
            if (await ActiveAsync(connection, s.Id, ct))
                await RestartedAsync(connection, s, next, ct);
    }

    private async Task RestartedAsync(NpgsqlConnection connection, SessionRow s, SessionRow next, CancellationToken ct)
    {
        var firstPrompt = await connection.QueryFirstOrDefaultAsync<string?>(new CommandDefinition(
            "SELECT text FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id AND kind = 'prompt' AND text IS NOT NULL ORDER BY at, seq LIMIT 1",
            new { Org, next.Id }, cancellationToken: ct));
        await store.Execute<SteeringState>(SteeringState.SessionStream(s.Id), state => SteeringDecider.Observe(state,
            [new SteeringEvents.Observed($"restart:{next.Id}", Signal.Restarted, Phase.InSession, Utc(next.StartedAt), s.Repo!, s.Id, null, s.Person, s.PersonMapped, s.Period,
                firstPrompt, Intent.Correction, new SteeringRefs(RestartedBy: next.Id))]), ct);
    }

    private async Task<bool> ActiveAsync(NpgsqlConnection connection, string sessionId, CancellationToken ct) =>
        await connection.ExecuteScalarAsync<bool>(new CommandDefinition(
            "SELECT EXISTS (SELECT 1 FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id AND kind IN ('response', 'tool_call'))",
            new { Org, Id = sessionId }, cancellationToken: ct));

    private sealed record PrRow(string Repo, int Number, bool IsAgent, DateTime? MergedAt, string? WorkItemId, string Snapshot, DateTime ChangedAt);

    private async Task<int> ScanPullRequestsAsync(string? repo, OrgSettings settings, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var rows = (await connection.QueryAsync<PrRow>(new CommandDefinition(
            $"""
            SELECT repo, number, is_agent, merged_at, work_item_id, snapshot::text AS snapshot, changed_at FROM casebox.pull_requests
            WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo) AND (steering_scanned_at IS NULL OR steering_scanned_at < changed_at)
            ORDER BY changed_at LIMIT {Batch}
            """,
            new { Org, Repo = repo }, cancellationToken: ct))).ToList();

        foreach (var row in rows)
        {
            var pr = JsonSerializer.Deserialize<PullRequestSnapshot>(row.Snapshot, GitHubJson.Options)!;
            var period = Identities.PeriodOf(settings.PseudonymPeriod, pr.CreatedAt);
            if (row.IsAgent)
            {
                var agentShas = await AgentCommitsAsync(connection, row.Repo, pr, ct);
                await CiFixesAsync(connection, row.Repo, pr, agentShas, period, ct);
                await EnqueuePullRequestJobAsync(row.Repo, pr, agentShas, ct);
            }

            if (row.MergedAt is { } merged) await AfterMergeAsync(connection, row, pr, Utc(merged), period, ct);

            await connection.ExecuteAsync(new CommandDefinition(
                "UPDATE casebox.pull_requests SET steering_scanned_at = @Seen WHERE org_id = @Org AND repo = @Repo AND number = @Number AND (steering_scanned_at IS NULL OR steering_scanned_at < @Seen)",
                new { Org, row.Repo, row.Number, Seen = row.ChangedAt }, cancellationToken: ct));
        }

        return rows.Count;
    }

    // docs/specs/steering.md, "Commits of a pull request".
    private async Task<HashSet<string>> AgentCommitsAsync(NpgsqlConnection connection, string repo, PullRequestSnapshot pr, CancellationToken ct)
    {
        var shas = pr.Commits.Select(c => c.Sha).ToArray();
        var agent = pr.Commits.Where(c => c.AgentCoAuthor).Select(c => c.Sha).ToHashSet(StringComparer.Ordinal);
        agent.UnionWith(await connection.QueryAsync<string>(new CommandDefinition(
            "SELECT DISTINCT sha FROM casebox.commit_attributions WHERE org_id = @Org AND repo = @Repo AND sha = ANY(@Shas)",
            new { Org, Repo = repo, Shas = shas }, cancellationToken: ct)));
        var windows = (await connection.QueryAsync<(DateTime Start, DateTime? End)>(new CommandDefinition(
            "SELECT started_at, ended_at FROM casebox.sessions WHERE org_id = @Org AND repo = @Repo AND branch = @Branch",
            new { Org, Repo = repo, Branch = pr.HeadRef }, cancellationToken: ct))).ToList();
        foreach (var c in pr.Commits)
            if (windows.Any(w => c.At >= Utc(w.Start) && c.At <= Utc(w.End ?? w.Start) + SessionCommitSlack))
                agent.Add(c.Sha);
        return agent;
    }

    private static bool Human(PrCommit c, HashSet<string> agentShas) => !agentShas.Contains(c.Sha) && c.Author is { Bot: false };

    // A failed check, then a human commit, then the same check passing.
    private async Task CiFixesAsync(NpgsqlConnection connection, string repo, PullRequestSnapshot pr, HashSet<string> agentShas, string period, CancellationToken ct)
    {
        var checks = (await connection.QueryAsync<(string Sha, string Name, string Conclusion)>(new CommandDefinition(
            "SELECT sha, name, conclusion FROM casebox.pr_checks WHERE org_id = @Org AND repo = @Repo AND number = @Number",
            new { Org, Repo = repo, pr.Number }, cancellationToken: ct)))
            .Concat(pr.Checks.Where(c => c.Conclusion is not null).Select(c => (Sha: c.HeadSha, c.Name, Conclusion: c.Conclusion!)))
            .Distinct().ToList();
        var order = pr.Commits.Select((c, i) => (c.Sha, i)).ToDictionary(x => x.Sha, x => x.i, StringComparer.Ordinal);
        var fixes = new List<SteeringEvents.Observed>();
        foreach (var check in checks.GroupBy(c => c.Name))
        {
            var passed = check.Where(c => c.Conclusion == "success" && order.ContainsKey(c.Sha)).Select(c => order[c.Sha]).ToList();
            foreach (var failure in check.Where(c => c.Conclusion is "failure" or "timed_out" && order.ContainsKey(c.Sha)))
            {
                var fix = pr.Commits.Skip(order[failure.Sha] + 1).FirstOrDefault(c => Human(c, agentShas));
                if (fix is null || !passed.Any(p => p >= order[fix.Sha])) continue;
                fixes.Add(new SteeringEvents.Observed($"ci:{fix.Sha}", Signal.CiFix, Phase.BeforeMerge, fix.At, repo, null, pr.Number, fix.Author!.Token, fix.Author.Mapped, period,
                    fix.Message, Intent.Correction, new SteeringRefs(Commits: [fix.Sha])));
            }
        }

        if (fixes.Count > 0)
            await store.Execute<SteeringState>(SteeringState.PullRequestStream(repo, pr.Number), s => SteeringDecider.Observe(s, fixes.DistinctBy(f => f.InterventionId)), ct);
    }

    // The worker finds human rewrites and review changes in its mirror (steering.pr).
    private async Task EnqueuePullRequestJobAsync(string repo, PullRequestSnapshot pr, HashSet<string> agentShas, CancellationToken ct)
    {
        var firstAgent = pr.Commits.Select((c, i) => (c, i)).FirstOrDefault(x => agentShas.Contains(x.c.Sha));
        var rewriteCandidates = firstAgent.c is not null && pr.Commits.Skip(firstAgent.i + 1).Any(c => Human(c, agentShas));
        var comments = pr.ReviewComments
            .Where(c => c.InReplyTo is null && c.Author is { Bot: false } && (c.OriginalLine ?? c.Line) is not null && (c.OriginalCommitSha ?? c.CommitSha) is not null)
            .Select(c => new { id = c.Id, path = c.Path, line = c.OriginalLine ?? c.Line, commitSha = c.OriginalCommitSha ?? c.CommitSha, at = c.At })
            .ToList();
        if (!rewriteCandidates && comments.Count == 0) return;

        var payload = new
        {
            repo, number = pr.Number, baseSha = pr.BaseSha, headSha = pr.HeadSha,
            commits = pr.Commits.Select(c => new { sha = c.Sha, agent = agentShas.Contains(c.Sha), at = c.At }),
            comments,
        };
        await using var connection = await db.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        await jobs.EnqueueAsync(transaction, Org, SteeringJobs.PullRequest, $"{SteeringJobs.PullRequest}:{repo}#{pr.Number}@{pr.HeadSha}:{comments.Count}", payload, 3, ct);
        await transaction.CommitAsync(ct);
    }

    // Reverts and fixes of earlier agent pull requests that this merged pull request makes.
    private async Task AfterMergeAsync(NpgsqlConnection connection, PrRow row, PullRequestSnapshot pr, DateTimeOffset merged, string period, CancellationToken ct)
    {
        if (pr.Author is not { Bot: false } author) return;
        var text = string.IsNullOrWhiteSpace(pr.Body) ? pr.Title : $"{pr.Title}\n\n{pr.Body}";
        var refs = new SteeringRefs(Commits: pr.MergeSha is null ? [] : [pr.MergeSha]);
        var self = $"{row.Repo}#{row.Number}";

        if (pr.Reverts is { } reverted && await MergedAgentAsync(connection, row.Repo, reverted, ct) is not null)
            await store.Execute<SteeringState>(SteeringState.PullRequestStream(row.Repo, reverted), s => SteeringDecider.Observe(s,
                [new SteeringEvents.Observed($"revert:pr:{self}", Signal.Revert, Phase.AfterMerge, merged, row.Repo, null, reverted, author.Token, author.Mapped, period, text, Intent.Correction, refs)]), ct);

        var fixedPrs = new List<(string Repo, int Number)>();
        foreach (var blamed in pr.Blamed)
            if (await MergedAgentAsync(connection, blamed.Repo, blamed.Number, ct) is { } at && at <= merged && merged - at <= PullRequestHandler.FixWindow)
                fixedPrs.Add((blamed.Repo, blamed.Number));

        if (row.WorkItemId is not null)
            fixedPrs.AddRange((await connection.QueryAsync<int>(new CommandDefinition(
                """
                SELECT number FROM casebox.pull_requests
                WHERE org_id = @Org AND repo = @Repo AND work_item_id = @Item AND is_agent AND number <> @Number AND merged_at < @Merged AND merged_at > @Since
                """,
                new { Org, row.Repo, Item = row.WorkItemId, row.Number, Merged = merged, Since = merged - PullRequestHandler.FixWindow }, cancellationToken: ct)))
                .Select(n => (row.Repo, n)));

        foreach (var (repo, number) in fixedPrs.Distinct().Where(p => p != (row.Repo, row.Number) && p.Number != pr.Reverts))
            await store.Execute<SteeringState>(SteeringState.PullRequestStream(repo, number), s => SteeringDecider.Observe(s,
                [new SteeringEvents.Observed($"fix:pr:{self}", Signal.Fix, Phase.AfterMerge, merged, repo, null, number, author.Token, author.Mapped, period, text, Intent.Correction, refs)]), ct);
    }

    private async Task<DateTimeOffset?> MergedAgentAsync(NpgsqlConnection connection, string repo, int number, CancellationToken ct)
    {
        var merged = await connection.QuerySingleOrDefaultAsync<DateTime?>(new CommandDefinition(
            "SELECT merged_at FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number AND is_agent",
            new { Org, Repo = repo, Number = number }, cancellationToken: ct));
        return merged is { } m ? Utc(m) : null;
    }

    private async Task EnqueueTaskAsync(string sessionId, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        await jobs.EnqueueAsync(transaction, Org, SteeringJobs.Classify, $"{SteeringJobs.Classify}:task:{sessionId}",
            new { stream = SteeringState.SessionStream(sessionId), interventionIds = Array.Empty<string>(), taskFor = sessionId }, 3, ct);
        await transaction.CommitAsync(ct);
    }

    // Pending interventions get a classification job, 20 at a time, unless a job for their
    // interventions is still waiting; a task-only job does not count. A failed job is retried by
    // the next day's key.
    private async Task EnqueuePendingAsync(CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var pending = (await connection.QueryAsync<(string Stream, string Id)>(new CommandDefinition(
            """
            SELECT f.stream_id, f.intervention_id FROM casebox.steering_facts f
            WHERE f.org_id = @Org AND f.status = 'pending'
              AND NOT EXISTS (SELECT 1 FROM casebox.jobs j WHERE j.org_id = @Org AND j.kind = @Kind AND j.status IN ('queued', 'leased') AND j.payload->>'stream' = f.stream_id
                              AND jsonb_array_length(j.payload->'interventionIds') > 0)
            ORDER BY f.stream_id, f.intervention_id
            """,
            new { Org, Kind = SteeringJobs.Classify }, cancellationToken: ct))).ToList();
        var day = clock.GetUtcNow().ToString("yyyy-MM-dd", System.Globalization.CultureInfo.InvariantCulture);
        foreach (var stream in pending.GroupBy(p => p.Stream))
        foreach (var chunk in stream.Select(p => p.Id).Chunk(SteeringJobs.PerJob))
        {
            await using var transaction = await connection.BeginTransactionAsync(ct);
            await jobs.EnqueueAsync(transaction, Org, SteeringJobs.Classify, $"{SteeringJobs.Classify}:{stream.Key}:{chunk[0]}:{chunk[^1]}:{chunk.Length}:{day}",
                new { stream = stream.Key, interventionIds = chunk }, 3, ct);
            await transaction.CommitAsync(ct);
        }
    }

    public static DateTimeOffset Utc(DateTime at) => new(DateTime.SpecifyKind(at, DateTimeKind.Utc));
}
