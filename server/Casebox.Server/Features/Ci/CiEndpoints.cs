using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Ci;

// casebox ci's routes (docs/specs/harness-ci.md). A ci token and a Member may call them.
public static class CiEndpoints
{
    public const string ResolveJob = "harness.resolve";

    // A baseline is scored again when its last complete score is older than this.
    public static readonly TimeSpan Freshness = TimeSpan.FromDays(7);

    public sealed record BaselineRequest(
        string Workspace,
        HarnessSpec Spec,
        int? Repeats,
        IReadOnlyDictionary<string, Price>? Prices,
        decimal? CapUsd,
        IReadOnlyList<string>? Globs,
        string? Shared
    );

    public sealed record PullRequestRequest(
        string? Workspace,
        string Repo,
        int Number,
        string HeadSha,
        string? BaseSha,
        HarnessSpec Spec,
        bool OnSharedHarness,
        int? Size,
        int? Repeats,
        IReadOnlyDictionary<string, Price>? Prices,
        string? ServerUrl,
        decimal? CapUsd,
        IReadOnlyList<string>? Globs,
        string? Shared
    );

    public sealed record RunRef(
        string Id,
        string? Workspace,
        string Status,
        string? EvaluationId,
        string? Message
    );

    public sealed record Started(IReadOnlyList<RunRef> Runs);

    public sealed record CaseRow(
        string CaseId,
        int BaselinePassed,
        int BaselineRuns,
        int CandidatePassed,
        int CandidateRuns,
        int FailedRuns,
        bool Regression
    );

    public sealed record BaselineInfo(string? HarnessHash, DateTimeOffset? ScoredAt);

    public sealed record RunView(
        string Id,
        string Kind,
        string? Workspace,
        string Repo,
        int? Number,
        string? HeadSha,
        string Status,
        string? Message,
        string? EvaluationId,
        string? Purpose,
        int Repeats,
        double Delta,
        Estimate? Estimate,
        decimal SpentUsd,
        decimal CapUsd,
        int RunsCompleted,
        int RunsFailed,
        EvaluationEvents.VerdictReached? Verdict,
        EvaluationEvents.Scored? Scored,
        string? Reason,
        IReadOnlyList<CaseRow> Cases,
        BaselineInfo? Baseline
    );

    public static void MapCi(this RouteGroupBuilder api)
    {
        var ci = api.MapGroup("/ci").WithTags("Harness CI");

        ci.MapPost(
                "/baselines",
                async (
                    BaselineRequest body,
                    HttpContext http,
                    NpgsqlDataSource db,
                    IEventStore store,
                    JobQueue jobs,
                    TimeProvider clock
                ) =>
                {
                    var org = http.User.OrgId();
                    var ct = http.RequestAborted;
                    if (body.Spec is null || string.IsNullOrWhiteSpace(body.Workspace))
                        throw new DomainException("A baseline names its workspace and spec.");
                    var spec = Normalize(body.Spec);
                    Planner.Check(spec, "baseline");
                    if (body.Repeats is < 1 or > 10)
                        throw new DomainException("A baseline runs 1 to 10 repeats.");
                    await ConfigureAsync(store, body.Workspace, body.Globs, body.Shared, ct);

                    await using var connection = await db.OpenConnectionAsync(ct);
                    var cases = (
                        await connection.QueryAsync<(string Id, string Repos)>(
                            new CommandDefinition(
                                """
                                SELECT id, repos::text FROM casebox.case_catalog
                                WHERE org_id = @Org AND workspace = @Workspace AND status = 'approved' AND split = 'dev' AND instruction IS NOT NULL
                                ORDER BY id
                                """,
                                new { Org = org, body.Workspace },
                                cancellationToken: ct
                            )
                        )
                    ).ToList();

                    var now = clock.GetUtcNow();
                    var id = Ids.New();
                    var run = new CiRun(
                        id,
                        CiRuns.Baseline,
                        body.Workspace,
                        spec.Shared?.Repo ?? "",
                        null,
                        null,
                        null,
                        null,
                        null,
                        cases.Count == 0 ? CiRuns.Skipped : CiRuns.Resolving,
                        cases.Count == 0
                            ? $"Workspace {body.Workspace} has no approved dev case to score."
                            : null,
                        JsonSerializer.Serialize(body with { Spec = spec }, EvaluationResults.Json),
                        http.User.Actor()!,
                        now.UtcDateTime
                    );
                    await using var transaction = await connection.BeginTransactionAsync(ct);
                    await CiRuns.InsertAsync(connection, transaction, org, run, ct);
                    if (cases.Count > 0)
                        await jobs.EnqueueAsync(
                            transaction,
                            org,
                            ResolveJob,
                            $"{ResolveJob}:{id}",
                            new
                            {
                                ciRun = id,
                                workspace = body.Workspace,
                                spec,
                                cases = cases.Select(c => new
                                {
                                    caseId = c.Id,
                                    repos = JsonDocument.Parse(c.Repos).RootElement,
                                }),
                            },
                            3,
                            ct
                        );
                    await transaction.CommitAsync(ct);
                    return Results.Accepted(
                        $"/api/v1/ci/runs/{id}",
                        new Started([new RunRef(id, body.Workspace, run.Status, null, run.Message)])
                    );
                }
            )
            .RequireAuthorization(Policies.CiOrMember);

        ci.MapPost(
                "/pull-requests",
                async (PullRequestRequest body, HttpContext http, CiPullRequests pulls) =>
                    Results.Ok(await pulls.StartAsync(body, http.User, http.RequestAborted))
            )
            .RequireAuthorization(Policies.CiOrMember);

        ci.MapGet(
                "/runs/{id}",
                async (string id, HttpContext http, NpgsqlDataSource db, IEventStore store) =>
                {
                    var org = http.User.OrgId();
                    var ct = http.RequestAborted;
                    await using var connection = await db.OpenConnectionAsync(ct);
                    if (
                        http.User.IsCiToken()
                        && !await CiRuns.OwnedByAsync(connection, org, id, http.User.TokenId()!, ct)
                    )
                        return Results.NotFound();
                    var run = await CiRuns.GetAsync(connection, null, org, id, ct);
                    return run is null
                        ? Results.NotFound()
                        : Results.Ok(await ViewAsync(connection, org, run, store, ct));
                }
            )
            .RequireAuthorization(Policies.CiOrViewer);
    }

    // Settings may be left out of a spec; every setting then takes its default.
    public static HarnessSpec Normalize(HarnessSpec spec) =>
        spec with
        {
            Settings = spec.Settings ?? new AgentSettings(null, null, null),
            Shared = spec.Shared is { } s
                ? s with
                {
                    Repo = WorkspaceDecider.NormalizeRepo(s.Repo),
                }
                : null,
        };

    // casebox.yml's harness globs and shared repository, recorded on the workspace when they change.
    public static async Task ConfigureAsync(
        IEventStore store,
        string workspace,
        IReadOnlyList<string>? globs,
        string? shared,
        CancellationToken ct
    )
    {
        if (globs is null)
        {
            var (w, _) = await store.Load<Workspace>(Workspace.StreamIdFor(workspace), ct);
            if (!w.Exists)
                throw new NotFoundException($"Workspace {workspace} does not exist.");
            return;
        }
        await store.Execute<Workspace>(
            Workspace.StreamIdFor(workspace),
            w => WorkspaceDecider.ConfigureHarness(w, globs, shared),
            ct
        );
    }

    public static async Task<RunView> ViewAsync(
        System.Data.Common.DbConnection connection,
        string org,
        CiRun run,
        IEventStore store,
        CancellationToken ct
    )
    {
        if (run.EvaluationId is null)
            return new RunView(
                run.Id,
                run.Kind,
                run.Workspace,
                run.Repo,
                run.Number,
                run.HeadSha,
                run.Status,
                run.Message,
                null,
                null,
                0,
                0,
                null,
                0,
                0,
                0,
                0,
                null,
                null,
                null,
                [],
                null
            );

        var (e, _) = await store.Load<Evaluation>(Evaluation.StreamId(run.EvaluationId), ct);
        var request = e.Request!;
        var row = await connection.QuerySingleAsync<(
            string Status,
            string? Verdict,
            string? Scored,
            string? Reason
        )>(
            new CommandDefinition(
                "SELECT status, verdict::text AS verdict, scored::text AS scored, reason FROM casebox.evaluations WHERE org_id = @Org AND id = @Id",
                new { Org = org, Id = run.EvaluationId },
                cancellationToken: ct
            )
        );
        var verdict = row.Verdict is null
            ? null
            : JsonSerializer.Deserialize<EvaluationEvents.VerdictReached>(
                row.Verdict,
                EvaluationResults.Json
            );
        var scored = row.Scored is null
            ? null
            : JsonSerializer.Deserialize<EvaluationEvents.Scored>(
                row.Scored,
                EvaluationResults.Json
            );
        var regressions = verdict?.Regressions ?? [];
        var scores = request.BaselineScores ?? new Dictionary<string, BaselineScore>();

        var cases = e
            .Cases.Select(c =>
            {
                var runs = e.Runs.Values.Where(r => r.CaseId == c.CaseId).ToList();
                var candidate = runs.Where(r => r.Side == Side.Candidate && !r.Failed).ToList();
                var baseline = scores.TryGetValue(c.CaseId, out var cached)
                    ? cached.Passed
                    : runs.Where(r => r.Side == Side.Baseline && !r.Failed)
                        .Select(r => r.Passed == true)
                        .ToList();
                return new CaseRow(
                    c.CaseId,
                    baseline.Count(p => p),
                    baseline.Count,
                    candidate.Count(r => r.Passed == true),
                    candidate.Count,
                    runs.Count(r => r.Failed),
                    regressions.Contains(c.CaseId)
                );
            })
            .ToList();

        var latest = scores.Values.OrderByDescending(s => s.ScoredAt).FirstOrDefault();
        return new RunView(
            run.Id,
            run.Kind,
            run.Workspace,
            run.Repo,
            run.Number,
            run.HeadSha,
            row.Status == "awaiting_confirmation" ? "waiting" : row.Status,
            run.Message,
            run.EvaluationId,
            Steering.SteeringFacts.Enum(request.Purpose),
            request.Repeats,
            request.Delta,
            request.Estimate,
            e.SpentUsd,
            e.CapUsd,
            e.Runs.Values.Count(r => !r.Failed),
            e.Runs.Values.Count(r => r.Failed),
            verdict,
            scored,
            row.Reason,
            cases,
            latest is null ? null : new BaselineInfo(latest.HarnessHash, latest.ScoredAt)
        );
    }
}
