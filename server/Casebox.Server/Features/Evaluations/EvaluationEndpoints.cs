using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Blobs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Evaluations;

public static class EvaluationEndpoints
{
    public sealed record EstimateView(
        Estimate Estimate,
        decimal CapUsd,
        bool NeedsConfirmation,
        decimal MonthSpentUsd,
        decimal MonthlyUsd,
        decimal ConfirmAboveUsd,
        string Change,
        bool MutableModel,
        int Cases
    );

    public sealed record Created(string Id, EstimateView Estimate);

    public sealed record CancelBody(string? Reason);

    public sealed record EvaluationRow(
        string Id,
        string Workspace,
        string Split,
        string Purpose,
        string Status,
        string Change,
        JsonElement Baseline,
        JsonElement Candidate,
        int Repeats,
        double Delta,
        decimal CapUsd,
        JsonElement Estimate,
        bool MutableModel,
        int Cases,
        decimal SpentUsd,
        int RunsCompleted,
        int RunsFailed,
        JsonElement? Verdict,
        string? Reason,
        DateTimeOffset CreatedAt,
        DateTimeOffset UpdatedAt,
        // A baseline evaluation's score, and the pull request or nightly run of harness CI.
        JsonElement? Scored,
        JsonElement? Ci
    );

    public sealed record CheckpointRow(
        int Round,
        double Level,
        int Cases,
        double Delta,
        double Lower,
        double Upper,
        string Verdict,
        DateTimeOffset At
    );

    public sealed record EvaluationDetail(
        EvaluationRow Evaluation,
        IReadOnlyList<CheckpointRow> Checkpoints
    );

    public sealed record CaseResult(
        string CaseId,
        double Weight,
        bool Drift,
        int BaselineRuns,
        int BaselinePassed,
        int CandidateRuns,
        int CandidatePassed,
        int FailedRuns,
        decimal BaselineCostUsd,
        decimal CandidateCostUsd,
        IReadOnlyList<string> Runs
    );

    public static void MapEvaluations(this RouteGroupBuilder api)
    {
        var evaluations = api.MapGroup("/evaluations")
            .WithTags("Evaluations")
            .RequireAuthorization(Policies.Viewer);

        evaluations
            .MapPost(
                "/estimate",
                async (EvaluationRequest body, Planner planner, HttpContext http) =>
                    Results.Ok(View(await planner.PlanAsync(body, http.RequestAborted)))
            )
            .RequireAuthorization(Policies.Member);

        // An evaluation shows its cost before it starts; above the threshold it waits for a person.
        evaluations
            .MapPost(
                "/",
                async (
                    EvaluationRequest body,
                    Planner planner,
                    IEventStore store,
                    HttpContext http
                ) =>
                {
                    var plan = await planner.PlanAsync(body, http.RequestAborted);
                    Planner.RequireMonthlyRoom(plan);
                    var id = Ids.New();
                    await store.Execute<Evaluation>(
                        Evaluation.StreamId(id),
                        e => EvaluationDecider.Request(e, plan.Requested),
                        http.RequestAborted
                    );
                    return Results.Created(
                        $"/api/v1/evaluations/{id}",
                        new Created(id, View(plan))
                    );
                }
            )
            .RequireAuthorization(Policies.Member);

        evaluations
            .MapPost(
                "/{id}/confirmation",
                async (string id, IEventStore store, HttpContext http) =>
                {
                    var by =
                        http.User.AccountId()
                        ?? throw new DomainException("Only an account confirms an evaluation.");
                    await store.Execute<Evaluation>(
                        Evaluation.StreamId(id),
                        e => EvaluationDecider.Confirm(e, by),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        evaluations
            .MapPost(
                "/{id}/cancellation",
                async (string id, CancelBody? body, IEventStore store, HttpContext http) =>
                {
                    var by = http.User.Actor() ?? "unknown";
                    await store.Execute<Evaluation>(
                        Evaluation.StreamId(id),
                        e => EvaluationDecider.Cancel(e, by, body?.Reason ?? ""),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        evaluations.MapGet(
            "/",
            async (string? workspace, int? limit, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var rows = await connection.QueryAsync<Row>(
                    new CommandDefinition(
                        $"SELECT {Columns} FROM casebox.evaluations WHERE org_id = @Org AND (@Workspace::text IS NULL OR workspace = @Workspace) ORDER BY created_at DESC LIMIT @Limit",
                        new
                        {
                            Org = http.User.OrgId(),
                            Workspace = workspace,
                            Limit = Math.Clamp(limit ?? 100, 1, 500),
                        },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(rows.Select(r => r.View()).ToList());
            }
        );

        evaluations.MapGet(
            "/{id}",
            async (string id, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var row = await RowAsync(connection, http, id);
                if (row is null)
                    return Results.NotFound();
                var checkpoints = await connection.QueryAsync<(
                    int Round,
                    double Level,
                    int Cases,
                    double Delta,
                    double Lower,
                    double Upper,
                    string Verdict,
                    DateTime At
                )>(
                    new CommandDefinition(
                        "SELECT round, level, cases, delta, lower, upper, verdict, at FROM casebox.evaluation_checkpoints WHERE org_id = @Org AND evaluation_id = @Id ORDER BY round",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(
                    new EvaluationDetail(
                        row.View(),
                        checkpoints
                            .Select(c => new CheckpointRow(
                                c.Round,
                                c.Level,
                                c.Cases,
                                c.Delta,
                                c.Lower,
                                c.Upper,
                                c.Verdict,
                                Utc(c.At)
                            ))
                            .ToList()
                    )
                );
            }
        );

        // Held-out evaluations answer only their verdict, counts and cost: the proposer never
        // sees held-out cases, their traces or their results (SDD section 9).
        evaluations.MapGet(
            "/{id}/cases",
            async (string id, HttpContext http, NpgsqlDataSource db, IEventStore store) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var row = await RowAsync(connection, http, id);
                if (row is null)
                    return Results.NotFound();
                if (row.Split == "held_out")
                    return HeldOut();
                var runs = await connection.QueryAsync<(
                    string CaseId,
                    string RunId,
                    string Side,
                    string Status,
                    bool? Passed,
                    decimal Cost,
                    double Weight,
                    bool Drift
                )>(
                    new CommandDefinition(
                        """
                        SELECT r.case_id, r.run_id, r.side, r.status, r.passed, r.cost_usd, coalesce(c.weight, 1), coalesce(c.drift, false)
                        FROM casebox.run_results r LEFT JOIN casebox.case_catalog c ON c.org_id = r.org_id AND c.id = r.case_id
                        WHERE r.org_id = @Org AND r.evaluation_id = @Id ORDER BY r.case_id, r.run_id
                        """,
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                // Harness CI's baseline side is the cached score it was requested with.
                var cached =
                    row.Purpose == "harness_ci"
                        ? (
                            await store.Load<Evaluation>(
                                Evaluation.StreamId(id),
                                http.RequestAborted
                            )
                        )
                            .State
                            .Request
                            ?.BaselineScores
                        : null;
                return Results.Ok(
                    runs.GroupBy(r => r.CaseId)
                        .Select(g =>
                        {
                            var done = g.Where(r => r.Status == "completed").ToList();
                            var b = done.Where(r => r.Side == "baseline").ToList();
                            var c = done.Where(r => r.Side == "candidate").ToList();
                            var score = cached?.GetValueOrDefault(g.Key);
                            return new CaseResult(
                                g.Key,
                                g.First().Weight,
                                g.First().Drift,
                                score?.Passed.Count ?? b.Count,
                                score?.Passed.Count(p => p) ?? b.Count(r => r.Passed == true),
                                c.Count,
                                c.Count(r => r.Passed == true),
                                g.Count(r => r.Status == "failed"),
                                score?.CostUsd.Sum() ?? b.Sum(r => r.Cost),
                                c.Sum(r => r.Cost),
                                g.Select(r => r.RunId).ToList()
                            );
                        })
                        .ToList()
                );
            }
        );

        evaluations.MapGet(
            "/{id}/runs/{runId}",
            async (
                string id,
                string runId,
                HttpContext http,
                NpgsqlDataSource db,
                BlobStore blobs
            ) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var row = await RowAsync(connection, http, id);
                if (row is null)
                    return Results.NotFound();
                if (row.Split == "held_out")
                    return HeldOut();
                var run = await connection.QuerySingleOrDefaultAsync<RunRow>(
                    new CommandDefinition(
                        """
                        SELECT run_id, case_id, side, repeat, status, passed, applied, usage::text AS usage, cost_usd, seconds, turns, tool_calls, model, timed_out,
                               token_cap_exceeded, process_checks::text AS process_checks, tests::text AS tests, failed_tests::text AS failed_tests,
                               assertions::text AS assertions, judge::text AS judge, reason, trace_blob
                        FROM casebox.run_results WHERE org_id = @Org AND evaluation_id = @Id AND run_id = @Run
                        """,
                        new
                        {
                            Org = http.User.OrgId(),
                            Id = id,
                            Run = runId,
                        },
                        cancellationToken: http.RequestAborted
                    )
                );
                if (run is null)
                    return Results.NotFound();
                JsonElement? trace = null;
                if (
                    run.TraceBlob is { } hash
                    && await blobs.GetAsync(http.User.OrgId(), hash, http.RequestAborted)
                        is { } blob
                )
                    trace = JsonDocument.Parse(blob.Data).RootElement.Clone();
                return Results.Ok(
                    new
                    {
                        run.RunId,
                        run.CaseId,
                        run.Side,
                        run.Repeat,
                        run.Status,
                        run.Passed,
                        run.Applied,
                        usage = Json(run.Usage),
                        run.CostUsd,
                        run.Seconds,
                        run.Turns,
                        run.ToolCalls,
                        run.Model,
                        run.TimedOut,
                        run.TokenCapExceeded,
                        processChecks = Json(run.ProcessChecks),
                        tests = Json(run.Tests),
                        failedTests = Json(run.FailedTests),
                        assertions = Json(run.Assertions),
                        judge = Json(run.Judge),
                        run.Reason,
                        trace,
                    }
                );
            }
        );

        // "Your harness vs no harness" is offered once a workspace has 10 approved dev cases and
        // no such evaluation yet (SDD section 8).
        evaluations.MapGet(
            "/offer",
            async (string workspace, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var (approved, done) = await connection.QuerySingleAsync<(int, bool)>(
                    new CommandDefinition(
                        """
                        SELECT (SELECT count(*) FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace AND status = 'approved' AND split = 'dev')::int,
                               EXISTS (SELECT 1 FROM casebox.evaluations WHERE org_id = @Org AND workspace = @Workspace AND purpose = 'harness_vs_none' AND status <> 'cancelled')
                        """,
                        new { Org = http.User.OrgId(), Workspace = workspace },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(
                    new
                    {
                        offer = approved >= EvaluationDecider.MinimumCases && !done,
                        approvedCases = approved,
                        done,
                    }
                );
            }
        );
    }

    private static IResult HeldOut() =>
        Results.Problem(
            statusCode: StatusCodes.Status403Forbidden,
            title: "A held-out evaluation shows only its verdict, counts and cost."
        );

    private static EstimateView View(Plan plan) =>
        new(
            plan.Requested.Estimate,
            plan.Requested.CapUsd,
            plan.Requested.NeedsConfirmation,
            plan.MonthSpentUsd,
            plan.MonthlyUsd,
            plan.ConfirmAboveUsd,
            plan.Requested.Change,
            plan.Requested.MutableModel,
            plan.Requested.Cases.Count
        );

    private static Task<Row?> RowAsync(NpgsqlConnection connection, HttpContext http, string id) =>
        connection.QuerySingleOrDefaultAsync<Row?>(
            new CommandDefinition(
                $"SELECT {Columns} FROM casebox.evaluations WHERE org_id = @Org AND id = @Id",
                new { Org = http.User.OrgId(), Id = id },
                cancellationToken: http.RequestAborted
            )
        );

    private static JsonElement? Json(string? text) =>
        text is null ? null : JsonDocument.Parse(text).RootElement.Clone();

    private static DateTimeOffset Utc(DateTime at) =>
        new(DateTime.SpecifyKind(at, DateTimeKind.Utc));

    private const string Columns = """
        id, workspace, split, purpose, status, change, baseline::text AS baseline, candidate::text AS candidate, repeats, delta, cap_usd,
        estimate::text AS estimate, mutable_model, cases, spent_usd, runs_completed, runs_failed, verdict::text AS verdict, reason, created_at, updated_at,
        scored::text AS scored,
        (SELECT jsonb_build_object('kind', c.kind, 'repo', c.repo, 'number', c.number, 'headSha', c.head_sha)::text
         FROM casebox.ci_runs c WHERE c.org_id = evaluations.org_id AND c.id = evaluations.ci_run) AS ci
        """;

    private sealed record Row(
        string Id,
        string Workspace,
        string Split,
        string Purpose,
        string Status,
        string Change,
        string Baseline,
        string Candidate,
        int Repeats,
        double Delta,
        decimal CapUsd,
        string Estimate,
        bool MutableModel,
        int Cases,
        decimal SpentUsd,
        int RunsCompleted,
        int RunsFailed,
        string? Verdict,
        string? Reason,
        DateTime CreatedAt,
        DateTime UpdatedAt,
        string? Scored,
        string? Ci
    )
    {
        public EvaluationRow View() =>
            new(
                Id,
                Workspace,
                Split,
                Purpose,
                Status,
                Change,
                Json(Baseline)!.Value,
                Json(Candidate)!.Value,
                Repeats,
                Delta,
                CapUsd,
                Json(Estimate)!.Value,
                MutableModel,
                Cases,
                SpentUsd,
                RunsCompleted,
                RunsFailed,
                Json(Verdict),
                Reason,
                Utc(CreatedAt),
                Utc(UpdatedAt),
                Json(Scored),
                Json(Ci)
            );
    }

    private sealed record RunRow(
        string RunId,
        string CaseId,
        string Side,
        int Repeat,
        string Status,
        bool? Passed,
        bool? Applied,
        string? Usage,
        decimal CostUsd,
        double? Seconds,
        int? Turns,
        int? ToolCalls,
        string? Model,
        bool TimedOut,
        bool TokenCapExceeded,
        string? ProcessChecks,
        string? Tests,
        string? FailedTests,
        string? Assertions,
        string? Judge,
        string? Reason,
        string? TraceBlob
    );
}
