using System.Data.Common;
using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.WorkItems;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Repos;

// The worker jobs that read a repository's extra refs: Entire checkpoints and git-ai notes.
public static class RepoJobKinds
{
    public const string Entire = "entire.fetch";
    public const string GitAi = "gitai.fetch";

    public static object Payload(string repo) => new { repo, sinceDays = 183 };

    // After a merge, both jobs run for the repository once for that merge.
    public static async Task EnqueueAfterMergeAsync(JobQueue queue, DbTransaction transaction, string org, string repo, string mergeSha, CancellationToken ct)
    {
        foreach (var kind in new[] { Entire, GitAi })
            await queue.EnqueueAsync(transaction, org, kind, $"{kind}:{repo}:{mergeSha}", Payload(repo), 3, ct);
    }
}

// The worker posts its findings through separate routes; the job result itself holds counts only,
// so these handlers append nothing.
public sealed class EntireFetchResult : IJobResultHandler
{
    public string Kind => RepoJobKinds.Entire;

    public Task HandleAsync(JobResult result, CancellationToken ct) => Task.CompletedTask;
}

public sealed class GitAiFetchResult : IJobResultHandler
{
    public string Kind => RepoJobKinds.GitAi;

    public Task HandleAsync(JobResult result, CancellationToken ct) => Task.CompletedTask;
}

public static class RepoJobEndpoints
{
    public sealed record AttributedCommit(string Sha, IReadOnlyList<AttributedFile> Files);

    public sealed record AttributedFile(string Path, string? Agent, string? Model, string Ranges);

    public sealed record Attributions(string Repo, IReadOnlyList<AttributedCommit> Commits);

    public static void MapRepoJobs(this RouteGroupBuilder worker)
    {
        // Entire sessions. A session Casebox already captured from local logs or hooks is skipped,
        // so its conversation is counted once.
        worker.MapPost("/sessions", async (CaptureBatch batch, HttpContext http, IEventStore store, CaptureStore capture, Linker linker, NpgsqlDataSource db) =>
        {
            if (batch.Session.Source != "entire") throw new DomainException("A worker posts Entire sessions only.");
            await using (var connection = await db.OpenConnectionAsync(http.RequestAborted))
            {
                var captured = await connection.ExecuteScalarAsync<bool>(new CommandDefinition(
                    """
                    SELECT EXISTS (SELECT 1 FROM casebox.session_events
                                   WHERE org_id = @Org AND session_id = @Id AND (seq < 2000000000))
                    """,
                    new { Org = http.User.OrgId(), Id = batch.Session.Id }, cancellationToken: http.RequestAborted));
                if (captured) return Results.Ok(new { stored = 0, skipped = true });
            }

            var (org, _) = await store.Load<Organisation>(Organisation.StreamId);
            var stored = await capture.StoreAsync(http.User.OrgId(), org.Settings, batch, http.RequestAborted);
            await linker.LinkSessionAsync(batch.Session.Id, http.RequestAborted);
            return Results.Ok(new { stored, skipped = false });
        }).WithTags("Worker").RequireAuthorization(Policies.Worker);

        // git-ai line attributions. A pull request with an attributed commit is an agent pull request.
        worker.MapPost("/attributions", async (Attributions body, HttpContext http, NpgsqlDataSource db, TimeProvider clock) =>
        {
            var repo = body.Repo.ToLowerInvariant();
            var rows = body.Commits.SelectMany(c => c.Files.Select(f => new
            {
                Org = http.User.OrgId(), Repo = repo, c.Sha, f.Path, Agent = f.Agent ?? "", Model = f.Model ?? "", Lines = f.Ranges, Now = clock.GetUtcNow(),
            })).ToList();
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            await using var transaction = await connection.BeginTransactionAsync(http.RequestAborted);
            await connection.ExecuteAsync(new CommandDefinition(
                """
                INSERT INTO casebox.commit_attributions (org_id, repo, sha, path, agent, model, agent_lines, created_at)
                VALUES (@Org, @Repo, @Sha, @Path, @Agent, @Model, @Lines, @Now) ON CONFLICT DO NOTHING
                """,
                rows, transaction, cancellationToken: http.RequestAborted));
            foreach (var commit in body.Commits)
                await connection.ExecuteAsync(new CommandDefinition(
                    "UPDATE casebox.pull_requests SET is_agent = true, changed_at = now() WHERE org_id = @Org AND repo = @Repo AND NOT is_agent AND snapshot->'commits' @> @Commit::jsonb",
                    new { Org = http.User.OrgId(), Repo = repo, Commit = JsonSerializer.Serialize(new[] { new { sha = commit.Sha } }) }, transaction, cancellationToken: http.RequestAborted));
            await transaction.CommitAsync(http.RequestAborted);
            return Results.Ok(new { stored = rows.Count });
        }).WithTags("Worker").RequireAuthorization(Policies.Worker);
    }
}

// Once a day, each workspace repository gets an Entire and a git-ai job.
public sealed class RepoJobScheduler(NpgsqlDataSource db, JobQueue queue, TimeProvider clock, ILogger<RepoJobScheduler> logger) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await EnqueueDailyAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogWarning(e, "Scheduling the daily repository jobs failed; it runs again in an hour.");
            }

            await Task.Delay(TimeSpan.FromHours(1), clock, stoppingToken);
        }
    }

    public async Task EnqueueDailyAsync(CancellationToken ct)
    {
        var day = clock.GetUtcNow().ToString("yyyy-MM-dd", System.Globalization.CultureInfo.InvariantCulture);
        await using var connection = await db.OpenConnectionAsync(ct);
        var repos = await connection.QueryAsync<(string Org, string Repo)>(new CommandDefinition(
            "SELECT DISTINCT org_id, jsonb_array_elements_text(repos) FROM casebox.workspaces", cancellationToken: ct));
        foreach (var (org, repo) in repos)
        {
            await using var transaction = await connection.BeginTransactionAsync(ct);
            foreach (var kind in new[] { RepoJobKinds.Entire, RepoJobKinds.GitAi })
                await queue.EnqueueAsync(transaction, org, kind, $"{kind}:{repo}:{day}", RepoJobKinds.Payload(repo), 3, ct);
            await transaction.CommitAsync(ct);
        }
    }
}
