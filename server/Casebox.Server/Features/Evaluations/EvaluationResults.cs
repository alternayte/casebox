using System.Text.Json;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Evaluations;

// The evaluation list, each evaluation's spend and progress, its checkpoints and its verdict.
// Inline, so a request reads its own writes.
public sealed class EvaluationResults : Projection
{
    public const string Name = "evaluation_results";

    internal static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        Converters = { Infrastructure.CaseboxStreams.Enums },
    };

    public EvaluationResults()
    {
        On<EvaluationEvents.Requested>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.evaluations (org_id, id, workspace, split, purpose, status, change, baseline, candidate, repeats, delta, cap_usd,
                        estimate, mutable_model, cases, created_at, updated_at)
                    VALUES (@Org, @Id, @Workspace, @Split, @Purpose, @Status, @Change, @Baseline::jsonb, @Candidate::jsonb, @Repeats, @Delta, @CapUsd,
                        @Estimate::jsonb, @MutableModel, @Cases, @At, @At)
                    ON CONFLICT DO NOTHING
                    """,
                    new
                    {
                        e.Workspace,
                        e.Split,
                        Purpose = SteeringFacts.Enum(e.Purpose),
                        Status = e.NeedsConfirmation ? "awaiting_confirmation" : "running",
                        e.Change,
                        Baseline = JsonSerializer.Serialize(e.Baseline, Json),
                        Candidate = JsonSerializer.Serialize(e.Candidate, Json),
                        e.Repeats,
                        e.Delta,
                        e.CapUsd,
                        Estimate = JsonSerializer.Serialize(e.Estimate, Json),
                        e.MutableModel,
                        Cases = e.Cases.Count,
                    }
                )
        );

        On<EvaluationEvents.Confirmed>(
            (_, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET status = 'running', updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { }
                )
        );

        On<EvaluationEvents.RunCompleted>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET spent_usd = spent_usd + @CostUsd, runs_completed = runs_completed + 1, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.CostUsd }
                )
        );

        On<EvaluationEvents.RunFailed>(
            (_, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET runs_failed = runs_failed + 1, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { }
                )
        );

        On<EvaluationEvents.CheckpointEvaluated>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.evaluation_checkpoints (org_id, evaluation_id, round, level, cases, delta, lower, upper, verdict, at)
                    VALUES (@Org, @Id, @Round, @Level, @Cases, @Delta, @Lower, @Upper, @Verdict, @At) ON CONFLICT DO NOTHING;
                    UPDATE casebox.evaluations SET updated_at = @At WHERE org_id = @Org AND id = @Id;
                    """,
                    new
                    {
                        e.Round,
                        e.Level,
                        e.Cases,
                        e.Delta,
                        e.Lower,
                        e.Upper,
                        Verdict = SteeringFacts.Enum(e.Verdict),
                    }
                )
        );

        On<EvaluationEvents.VerdictReached>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET status = 'done', verdict = @Verdict::jsonb, reason = coalesce(@Reason, reason), updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Verdict = JsonSerializer.Serialize(e, Json), e.Reason }
                )
        );

        On<EvaluationEvents.BudgetExhausted>(
            (_, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET reason = 'budget', updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { }
                )
        );

        On<EvaluationEvents.Cancelled>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.evaluations SET status = 'cancelled', reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Reason }
                )
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.evaluation_checkpoints; DELETE FROM casebox.evaluations;",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId["evaluation:".Length..]);
        parameters.Add("At", ctx.OccurredAt);
        return ctx.Connection.ExecuteAsync(
            new CommandDefinition(
                sql,
                parameters,
                ctx.Transaction,
                cancellationToken: ctx.CancellationToken
            )
        );
    }
}
