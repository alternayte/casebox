using System.Text.Json;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Evaluations;

public static class EvaluationJobs
{
    public const string Run = "run";
    public const string Verify = "verify";
}

// The evaluation stream is the workflow (SDD section 10): this subscription reacts to its events.
// It starts a round when the evaluation may run, and when a round's runs have all finished it
// records the checkpoint, then the verdict or the next round. Delivery is at least once: job keys
// and the decider make every step idempotent.
public sealed class EvaluationWorkflow : Subscription
{
    public const string Name = "evaluation_workflow";

    public EvaluationWorkflow()
    {
        On<EvaluationEvents.Requested>(
            (e, ctx) => e.NeedsConfirmation ? Task.CompletedTask : Step(ctx)
        );
        On<EvaluationEvents.Confirmed>((_, ctx) => Step(ctx));
        On<EvaluationEvents.RunCompleted>((_, ctx) => Step(ctx));
        On<EvaluationEvents.RunFailed>((_, ctx) => Step(ctx));
        On<EvaluationEvents.BudgetExhausted>((_, ctx) => Step(ctx));
        On<EvaluationEvents.Cancelled>((_, ctx) => Cancel(ctx));
    }

    private static Task Step(SubscriptionContext ctx) =>
        ctx
            .Services.GetRequiredService<EvaluationSteps>()
            .AdvanceAsync(ctx.Envelope.StreamId, ctx.CancellationToken);

    // Runs that have not started do not start: their queued jobs fail with the reason.
    private static async Task Cancel(SubscriptionContext ctx)
    {
        var db = ctx.Services.GetRequiredService<NpgsqlDataSource>();
        await using var connection = await db.OpenConnectionAsync(ctx.CancellationToken);
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.jobs SET status = 'failed', last_error = 'The evaluation was cancelled.', updated_at = now()
                WHERE org_id = @Org AND kind IN ('run', 'verify') AND status = 'queued' AND payload->>'evaluationId' = @Id
                """,
                new
                {
                    Org = ctx.Envelope.TenantId,
                    Id = ctx.Envelope.StreamId["evaluation:".Length..],
                },
                cancellationToken: ctx.CancellationToken
            )
        );
    }
}

public sealed class EvaluationSteps(
    IEventStore store,
    NpgsqlDataSource db,
    JobQueue jobs,
    DeedboxContext context
)
{
    private string Org => context.TenantId;

    public async Task AdvanceAsync(string streamId, CancellationToken ct)
    {
        var id = streamId["evaluation:".Length..];
        var (e, _) = await store.Load<Evaluation>(streamId, ct);
        if (
            !e.Exists
            || e.Verdict
            || e.Status is EvaluationStatus.Cancelled or EvaluationStatus.AwaitingConfirmation
        )
            return;

        var round = e.Checkpoints.Count + 1;
        if (e.Exhausted)
        {
            var spent = Evaluate(e, id, round, Statistics.Level(e.Repeats, e.Repeats));
            await store.Execute<Evaluation>(
                streamId,
                s =>
                    EvaluationDecider.Conclude(
                        s,
                        Verdict(s, spent, Statistics.Verdict.Inconclusive, "budget")
                    ),
                ct
            );
            return;
        }

        // A round with no recorded run yet is started; starting it twice enqueues nothing new.
        if (!e.Runs.Values.Any(r => r.Repeat == round))
        {
            await StartRoundAsync(e, id, round, ct);
            return;
        }

        if (!e.RoundDone(round))
            return;

        var level = Statistics.Level(round, e.Repeats);
        var checkpoint = Evaluate(e, id, round, level);
        await store.Execute<Evaluation>(
            streamId,
            s =>
                EvaluationDecider.Checkpoint(
                    s,
                    new EvaluationEvents.CheckpointEvaluated(
                        round,
                        level,
                        checkpoint.Cases,
                        checkpoint.Delta,
                        checkpoint.Lower,
                        checkpoint.Upper,
                        checkpoint.Verdict
                    )
                ),
            ct
        );

        if (checkpoint.Verdict != Statistics.Verdict.Inconclusive || round >= e.Repeats)
        {
            await store.Execute<Evaluation>(
                streamId,
                s =>
                    EvaluationDecider.Conclude(
                        s,
                        Verdict(
                            s,
                            checkpoint,
                            checkpoint.Verdict,
                            Statistics.InconclusiveReason(
                                checkpoint.Verdict,
                                checkpoint.Lower,
                                checkpoint.Upper,
                                s.Delta,
                                checkpoint.Cases
                            )
                        )
                    ),
                ct
            );
            return;
        }

        // A round starts only when its estimate fits under the cap.
        if (e.SpentUsd + e.Request!.Estimate.PerRoundUsd > e.CapUsd)
        {
            await store.Execute<Evaluation>(
                streamId,
                s =>
                    s.Exhausted
                        ? []
                        : new object[] { new EvaluationEvents.BudgetExhausted(s.SpentUsd) }.Concat(
                            EvaluationDecider.Conclude(
                                s,
                                Verdict(s, checkpoint, Statistics.Verdict.Inconclusive, "budget")
                            )
                        ),
                ct
            );
            return;
        }

        await StartRoundAsync(e, id, round + 1, ct);
    }

    private static Statistics.Checkpoint Evaluate(Evaluation e, string id, int round, double level)
    {
        var cases = e
            .Cases.Select(c =>
            {
                var runs = e
                    .Runs.Values.Where(r => r.CaseId == c.CaseId && !r.Failed && r.Repeat <= round)
                    .ToList();
                var b = runs.Where(r => r.Side == Side.Baseline).ToList();
                var k = runs.Where(r => r.Side == Side.Candidate).ToList();
                return new Statistics.CaseRuns(
                    c.CaseId,
                    c.Weight,
                    b.Select(r => r.Passed == true).ToList(),
                    k.Select(r => r.Passed == true).ToList(),
                    b.Select(r => (double)r.CostUsd).ToList(),
                    k.Select(r => (double)r.CostUsd).ToList(),
                    b.Select(r => r.Seconds).ToList(),
                    k.Select(r => r.Seconds).ToList()
                );
            })
            .ToList();
        return Statistics.Evaluate(
            cases,
            level,
            e.Delta,
            Statistics.Resamples,
            Statistics.SeedOf(id) + (ulong)round
        );
    }

    private static EvaluationEvents.VerdictReached Verdict(
        Evaluation e,
        Statistics.Checkpoint c,
        Statistics.Verdict verdict,
        string? reason
    ) =>
        new(
            verdict,
            c.Delta,
            c.Lower,
            c.Upper,
            c.Level,
            c.Cases,
            e.Runs.Values.Count(r => !r.Failed),
            c.CostRatio,
            c.CostLower,
            c.CostUpper,
            c.DurationRatio,
            c.DurationLower,
            c.DurationUpper,
            verdict == Statistics.Verdict.Equivalent && c.EquivalentAndCheaper,
            c.BaselineRate,
            c.CandidateRate,
            reason
        );

    // One repeat of every case on both sides, in a random interleaved order seeded by the
    // evaluation and the round, so provider drift during the round hits both sides alike.
    private async Task StartRoundAsync(Evaluation e, string id, int round, CancellationToken ct)
    {
        var request = e.Request!;
        var (workspace, _) = await store.Load<Workspace>(
            Workspace.StreamIdFor(request.Workspace),
            ct
        );
        await using var connection = await db.OpenConnectionAsync(ct);
        var cases = (
            await connection.QueryAsync<(
                string Id,
                string Repos,
                string? Oracle,
                string? Instruction,
                string? Assertions,
                string? Judge
            )>(
                new CommandDefinition(
                    "SELECT id, repos::text, oracle, instruction, CASE WHEN assertions_approved THEN assertions::text END AS assertions, CASE WHEN assertions_approved THEN judge::text END AS judge FROM casebox.case_catalog WHERE org_id = @Org AND id = ANY(@Ids)",
                    new { Org, Ids = e.Cases.Select(c => c.CaseId).ToArray() },
                    cancellationToken: ct
                )
            )
        ).ToDictionary(c => c.Id);

        var order = e.RoundRuns(round).ToList();
        var rng = new Random((int)(Statistics.SeedOf(id) % int.MaxValue) + round);
        for (var i = order.Count - 1; i > 0; i--)
        {
            var j = rng.Next(i + 1);
            (order[i], order[j]) = (order[j], order[i]);
        }

        await using var transaction = await connection.BeginTransactionAsync(ct);
        foreach (var (runId, caseId, side) in order)
        {
            if (!cases.TryGetValue(caseId, out var c) || c.Oracle is null || c.Instruction is null)
                continue;
            var spec = side == Side.Baseline ? request.Baseline : request.Candidate;
            var payload = JsonSerializer.SerializeToElement(
                new
                {
                    evaluationId = id,
                    runId,
                    caseId,
                    side,
                    repeat = round,
                    spec,
                    recipe = JsonDocument.Parse(workspace.Recipe!).RootElement,
                    recipeHash = workspace.RecipeHash,
                    repos = JsonDocument.Parse(c.Repos).RootElement,
                    instruction = Masking.Mask(c.Instruction),
                    oracle = c.Oracle,
                    // Only person-approved steering assertions and judge questions decide a run.
                    assertions = c.Assertions is null
                        ? (JsonElement?)null
                        : JsonDocument.Parse(c.Assertions).RootElement,
                    judge = c.Judge is null
                        ? (JsonElement?)null
                        : JsonDocument.Parse(c.Judge).RootElement,
                    prices = request.Prices,
                },
                EvaluationResults.Json
            );
            await jobs.EnqueueAsync(
                transaction,
                Org,
                EvaluationJobs.Run,
                $"{EvaluationJobs.Run}:{id}:{runId}",
                payload,
                3,
                ct
            );
        }

        await transaction.CommitAsync(ct);
    }
}
