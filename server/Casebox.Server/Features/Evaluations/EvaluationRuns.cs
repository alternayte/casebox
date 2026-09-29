using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Evaluations;

public sealed record RunUsage(
    long? InputTokens,
    long? OutputTokens,
    long? CacheReadTokens,
    long? CacheWriteTokens,
    decimal? CostUsd
)
{
    // As the token cap counts them: cache reads cost about a tenth of input and are left out.
    public long Total => (InputTokens ?? 0) + (OutputTokens ?? 0) + (CacheWriteTokens ?? 0);
}

public sealed record ProcessChecks(bool RanTestsBeforeDone, bool EditedTestAfterFailure);

// The worker's answer to a run job (docs/specs/evaluations.md, `run`).
public sealed record RunAnswer(
    string RunId,
    string? Diff,
    string? Log,
    string? Trace,
    RunUsage? Usage,
    decimal CostUsd,
    double Seconds,
    int Turns,
    int ToolCalls,
    string? Model,
    bool TimedOut,
    bool TokenCapExceeded,
    ProcessChecks? ProcessChecks,
    string? HarnessHash
);

public sealed record TestCount(int Passed, int Total);

public sealed record TestCounts(TestCount FailToPass, TestCount PassToPass);

public sealed record AssertionResult(string Kind, bool Passed);

public sealed record JudgeAnswer(string Answer, string Evidence);

// The worker's answer to a verify job (docs/specs/evaluations.md, `verify`).
public sealed record VerifyAnswer(
    string RunId,
    bool Applied,
    TestCounts? Tests,
    IReadOnlyList<string>? Failed,
    IReadOnlyList<AssertionResult>? Assertions,
    JudgeAnswer? Judge,
    bool Passed
);

// Records what the agent did, and queues its verification in a separate sandbox.
public sealed class RunResultHandler(IServiceProvider services, TimeProvider clock)
    : IJobResultHandler
{
    public string Kind => EvaluationJobs.Run;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var payload = result.Job.Payload;
        var evaluationId = payload.GetProperty("evaluationId").GetString()!;
        var answer =
            result.Result.Deserialize<RunAnswer>(EvaluationResults.Json)
            ?? throw new DomainException("The result is empty.");
        var spec = payload.GetProperty("spec").Deserialize<HarnessSpec>(EvaluationResults.Json)!;
        var connection = result.Transaction.Connection!;
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.run_results (org_id, evaluation_id, run_id, case_id, side, repeat, status, usage, cost_usd, seconds, turns, tool_calls, model,
                    timed_out, token_cap_exceeded, process_checks, harness_hash, diff_blob, log_blob, trace_blob, created_at)
                VALUES (@Org, @Evaluation, @Run, @Case, @Side, @Repeat, 'ran', @Usage::jsonb, @Cost, @Seconds, @Turns, @ToolCalls, @Model,
                    @TimedOut, @Capped, @Checks::jsonb, @Harness, @Diff, @Log, @Trace, @Now)
                ON CONFLICT (org_id, evaluation_id, run_id) DO NOTHING
                """,
                new
                {
                    Org = result.OrgId,
                    Evaluation = evaluationId,
                    Run = answer.RunId,
                    Case = payload.GetProperty("caseId").GetString(),
                    Side = payload.GetProperty("side").GetString(),
                    Repeat = payload.GetProperty("repeat").GetInt32(),
                    Usage = JsonSerializer.Serialize(answer.Usage, EvaluationResults.Json),
                    Cost = answer.CostUsd,
                    answer.Seconds,
                    answer.Turns,
                    answer.ToolCalls,
                    answer.Model,
                    answer.TimedOut,
                    Capped = answer.TokenCapExceeded,
                    Checks = JsonSerializer.Serialize(answer.ProcessChecks, EvaluationResults.Json),
                    Harness = answer.HarnessHash,
                    answer.Diff,
                    answer.Log,
                    answer.Trace,
                    Now = clock.GetUtcNow(),
                },
                result.Transaction,
                cancellationToken: ct
            )
        );

        if (answer.HarnessHash is { Length: 64 } harness)
            await HarnessVersionAsync(
                connection,
                result,
                harness,
                spec,
                answer.Model ?? spec.Model,
                ct
            );

        var (evaluation, _) = await result.Store.Load<Evaluation>(
            Evaluation.StreamId(evaluationId),
            ct
        );
        if (!evaluation.Open)
            return;
        var verify = JsonSerializer.SerializeToElement(
            new
            {
                evaluationId,
                runId = answer.RunId,
                caseId = payload.GetProperty("caseId").GetString(),
                recipe = payload.GetProperty("recipe"),
                repos = payload.GetProperty("repos"),
                oracle = payload.GetProperty("oracle").GetString(),
                diff = answer.Diff,
                trace = answer.Trace,
                instruction = payload.GetProperty("instruction").GetString(),
                assertions = payload.TryGetProperty("assertions", out var assertions)
                    ? assertions
                    : (JsonElement?)null,
                judge = payload.TryGetProperty("judge", out var judge) ? judge : (JsonElement?)null,
            },
            EvaluationResults.Json
        );
        await services
            .GetRequiredService<JobQueue>()
            .EnqueueAsync(
                result.Transaction,
                result.OrgId,
                EvaluationJobs.Verify,
                $"{EvaluationJobs.Verify}:{evaluationId}:{answer.RunId}",
                verify,
                3,
                ct
            );
    }

    // The harness version of a run: its harness files' hash, the agent and the model it reported.
    private static Task HarnessVersionAsync(
        System.Data.Common.DbConnection connection,
        JobResult result,
        string filesHash,
        HarnessSpec spec,
        string model,
        CancellationToken ct
    )
    {
        var agent = $"{spec.Agent}@{spec.AgentVersion}";
        var version = Convert.ToHexStringLower(
            SHA256.HashData(Encoding.UTF8.GetBytes($"{filesHash}\n{agent}\n{model}"))
        );
        return connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.harness_versions (org_id, hash, files_hash, files, agent, model, created_at)
                VALUES (@Org, @Version, @Files, '[]', @Agent, @Model, now()) ON CONFLICT DO NOTHING
                """,
                new
                {
                    Org = result.OrgId,
                    Version = version,
                    Files = filesHash,
                    Agent = agent,
                    Model = model,
                },
                result.Transaction,
                cancellationToken: ct
            )
        );
    }
}

// Records the verification and the run's completion on the evaluation stream in one transaction.
public sealed class VerifyResultHandler(TimeProvider clock) : IJobResultHandler
{
    public string Kind => EvaluationJobs.Verify;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var evaluationId = result.Job.Payload.GetProperty("evaluationId").GetString()!;
        var answer =
            result.Result.Deserialize<VerifyAnswer>(EvaluationResults.Json)
            ?? throw new DomainException("The result is empty.");
        var connection = result.Transaction.Connection!;

        // A judge answer can fail a run whose tests passed; it never passes a run whose tests or
        // assertions failed (SDD section 7).
        var deterministic =
            answer.Applied
            && answer.Tests is { } t
            && t.FailToPass.Passed == t.FailToPass.Total
            && t.PassToPass.Passed == t.PassToPass.Total
            && (answer.Assertions ?? []).All(a => a.Passed);
        var judgeFailed =
            answer.Judge is { Answer: var a }
            && !a.Equals("yes", StringComparison.OrdinalIgnoreCase);
        var passed = deterministic && !judgeFailed;
        var evidence = deterministic && judgeFailed ? "medium" : "strong";

        var run = await connection.QuerySingleOrDefaultAsync<(
            string CaseId,
            string Side,
            int Repeat,
            decimal Cost,
            double? Seconds,
            string? Usage
        )?>(
            new CommandDefinition(
                """
                UPDATE casebox.run_results SET status = 'completed', passed = @Passed, applied = @Applied, tests = @Tests::jsonb, failed_tests = @Failed::jsonb,
                    assertions = @Assertions::jsonb, judge = @Judge::jsonb, completed_at = @Now
                WHERE org_id = @Org AND evaluation_id = @Evaluation AND run_id = @Run
                RETURNING case_id, side, repeat, cost_usd, seconds, usage::text
                """,
                new
                {
                    Org = result.OrgId,
                    Evaluation = evaluationId,
                    Run = answer.RunId,
                    Passed = passed,
                    answer.Applied,
                    Tests = JsonSerializer.Serialize(answer.Tests, EvaluationResults.Json),
                    Failed = JsonSerializer.Serialize(answer.Failed ?? [], EvaluationResults.Json),
                    Assertions = JsonSerializer.Serialize(
                        answer.Assertions ?? [],
                        EvaluationResults.Json
                    ),
                    Judge = JsonSerializer.Serialize(answer.Judge, EvaluationResults.Json),
                    Now = clock.GetUtcNow(),
                },
                result.Transaction,
                cancellationToken: ct
            )
        );
        if (run is not { } r)
            throw new DomainException("The run has no recorded result.");

        var stream = Evaluation.StreamId(evaluationId);
        var (evaluation, _) = await result.Store.Load<Evaluation>(stream, ct);
        if (!evaluation.Open)
            return; // a verdict or a cancel came first: the result is kept, the stream records no more runs
        var tokens = r.Usage is null
            ? 0
            : JsonSerializer.Deserialize<RunUsage>(r.Usage, EvaluationResults.Json)?.Total ?? 0;
        var side = SteeringFacts.Parse<Side>(r.Side)!.Value;
        await result.Store.Execute<Evaluation>(
            stream,
            s =>
                EvaluationDecider.CompleteRun(
                    s,
                    new EvaluationEvents.RunCompleted(
                        answer.RunId,
                        r.CaseId,
                        side,
                        r.Repeat,
                        passed,
                        r.Cost,
                        r.Seconds ?? 0,
                        tokens,
                        evidence
                    )
                ),
            ct
        );
    }
}

// A run whose job failed for good (after its retries, or an expired last lease) is recorded as
// run_failed, so its round can finish.
public sealed class RunFailureSweeper(
    IServiceScopeFactory scopes,
    NpgsqlDataSource db,
    TimeProvider clock,
    ILogger<RunFailureSweeper> logger
) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await SweepAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogError(e, "Recording failed runs failed; it runs again in a minute.");
            }

            await Task.Delay(TimeSpan.FromMinutes(1), clock, stoppingToken);
        }
    }

    public async Task SweepAsync(CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var failed = await connection.QueryAsync<(
            string Org,
            string Evaluation,
            string Run,
            string CaseId,
            string Side,
            int Repeat,
            string? Error
        )>(
            new CommandDefinition(
                """
                SELECT j.org_id, j.payload->>'evaluationId', j.payload->>'runId', coalesce(j.payload->>'caseId', r.case_id),
                       coalesce(j.payload->>'side', r.side), coalesce((j.payload->>'repeat')::int, r.repeat), j.last_error
                FROM casebox.jobs j
                LEFT JOIN casebox.run_results r ON r.org_id = j.org_id AND r.evaluation_id = j.payload->>'evaluationId' AND r.run_id = j.payload->>'runId'
                WHERE j.kind IN ('run', 'verify') AND j.status = 'failed' AND coalesce(r.status, 'none') <> 'completed' AND coalesce(r.status, 'none') <> 'failed'
                """,
                cancellationToken: ct
            )
        );
        foreach (var f in failed)
        {
            await using var scope = scopes.CreateAsyncScope();
            var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
            context.TenantId = f.Org;
            context.Metadata = new EventMetadata { Actor = "system:evaluation" };
            var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
            await using var transaction = await connection.BeginTransactionAsync(ct);
            await connection.ExecuteAsync(
                new CommandDefinition(
                    """
                    INSERT INTO casebox.run_results (org_id, evaluation_id, run_id, case_id, side, repeat, status, reason, created_at)
                    VALUES (@Org, @Evaluation, @Run, @Case, @Side, @Repeat, 'failed', @Reason, @Now)
                    ON CONFLICT (org_id, evaluation_id, run_id) DO UPDATE SET status = 'failed', reason = EXCLUDED.reason
                    """,
                    new
                    {
                        f.Org,
                        f.Evaluation,
                        f.Run,
                        Case = f.CaseId,
                        f.Side,
                        f.Repeat,
                        Reason = f.Error ?? "failed",
                        Now = clock.GetUtcNow(),
                    },
                    transaction,
                    cancellationToken: ct
                )
            );
            var stream = Evaluation.StreamId(f.Evaluation);
            var (evaluation, _) = await store.Load<Evaluation>(stream, ct);
            if (evaluation.Open)
                await store
                    .UseTransaction(transaction)
                    .Execute<Evaluation>(
                        stream,
                        s =>
                            EvaluationDecider.FailRun(
                                s,
                                new EvaluationEvents.RunFailed(
                                    f.Run,
                                    f.CaseId,
                                    SteeringFacts.Parse<Side>(f.Side)!.Value,
                                    f.Repeat,
                                    f.Error ?? "failed"
                                )
                            ),
                        ct
                    );
            await transaction.CommitAsync(ct);
        }
    }
}
