using System.Text.Json;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Cases;

// The case catalog: one row per case with its status, oracle counts, instruction and split.
// Inline, so the review queue reads its own writes. It scrubs the instruction when its person is
// erased.
public sealed class CaseCatalog : Projection
{
    public const string Name = "case_catalog";

    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        Converters = { Infrastructure.CaseboxStreams.Enums },
    };

    public CaseCatalog()
    {
        On<CaseEvents.Mined>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.case_catalog (org_id, id, kind, workspace, status, scope, source, work_item, repos, rank, recipe_hash, harness_hash, mined_at, updated_at)
                    VALUES (@Org, @Id, @Kind, @Workspace, 'mined', @Scope, @Source, @WorkItem, @Repos::jsonb, @Rank, @RecipeHash, @HarnessHash, @At, @At)
                    ON CONFLICT DO NOTHING
                    """,
                    new
                    {
                        Kind = SteeringFacts.Enum(e.Kind),
                        e.Workspace,
                        Scope = SteeringFacts.Enum(e.Scope),
                        e.Source,
                        e.WorkItem,
                        Repos = JsonSerializer.Serialize(e.Repos, Json),
                        e.Rank,
                        e.RecipeHash,
                        e.HarnessHash,
                    }
                )
        );

        On<CaseEvents.Validated>(
            async (e, ctx) =>
            {
                await Run(ctx, true, null, null, e.FailToPass, e.PassToPass, e.Seconds);
                await Exec(
                    ctx,
                    """
                    UPDATE casebox.case_catalog SET oracle = @Oracle, fail_to_pass = @FailToPass, pass_to_pass = @PassToPass, seconds = @Seconds,
                        drift = @Drift, weight = @Weight, failure_reason = NULL, failure_detail = NULL,
                        status = CASE WHEN status = 'approved' THEN status ELSE 'validated' END, updated_at = @At
                    WHERE org_id = @Org AND id = @Id
                    """,
                    new
                    {
                        e.Oracle,
                        e.FailToPass,
                        e.PassToPass,
                        e.Seconds,
                        e.Drift,
                        e.Weight,
                    }
                );
            }
        );

        On<CaseEvents.ValidationFailed>(
            async (e, ctx) =>
            {
                await Run(ctx, false, SteeringFacts.Enum(e.Reason), e.Detail, null, null, null);
                await Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET status = 'validation_failed', failure_reason = @Reason, failure_detail = @Detail, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Reason = SteeringFacts.Enum(e.Reason), e.Detail }
                );
            }
        );

        On<CaseEvents.InstructionDrafted>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    UPDATE casebox.case_catalog SET instruction = @Text, person = @Person, signatures = @Signatures::jsonb,
                        assertions = @Assertions::jsonb, judge = @Judge::jsonb, updated_at = @At
                    WHERE org_id = @Org AND id = @Id
                    """,
                    new
                    {
                        e.Text,
                        e.Person,
                        Signatures = JsonSerializer.Serialize(e.Signatures, Json),
                        Assertions = JsonSerializer.Serialize(e.Assertions, Json),
                        Judge = JsonSerializer.Serialize(e.Judge, Json),
                    }
                )
        );

        On<CaseEvents.InstructionEdited>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET instruction = @Text, person = @Person, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Text, e.Person }
                )
        );

        On<CaseEvents.AssertionsApproved>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    UPDATE casebox.case_catalog SET assertions = @Assertions::jsonb, judge = @Judge::jsonb, assertions_approved = true, updated_at = @At
                    WHERE org_id = @Org AND id = @Id
                    """,
                    new
                    {
                        Assertions = JsonSerializer.Serialize(e.Assertions, Json),
                        Judge = JsonSerializer.Serialize(e.Judge, Json),
                    }
                )
        );

        On<CaseEvents.Approved>(
            (_, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET status = 'approved', updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { }
                )
        );

        On<CaseEvents.Rejected>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET status = 'rejected', reject_reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Reason }
                )
        );

        On<CaseEvents.SplitAssigned>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET split = @Split, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Split = SteeringFacts.Enum(e.Split) }
                )
        );

        On<CaseEvents.Retired>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET status = 'retired', retired_reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Reason = SteeringFacts.Enum(e.Reason) }
                )
        );

        On<SubjectErased>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.case_catalog SET instruction = NULL, updated_at = @At WHERE org_id = @Org AND id = @Id AND person = @Subject",
                    new { Subject = e.SubjectId }
                )
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.case_catalog; DELETE FROM casebox.case_validations;",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task Run(
        ProjectionContext ctx,
        bool passed,
        string? reason,
        string? detail,
        int? failToPass,
        int? passToPass,
        double? seconds
    ) =>
        ctx.Connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.case_validations (org_id, case_id, event_id, at, passed, reason, detail, fail_to_pass, pass_to_pass, seconds)
                VALUES (@Org, @Id, @Event, @At, @Passed, @Reason, @Detail, @FailToPass, @PassToPass, @Seconds) ON CONFLICT DO NOTHING
                """,
                new
                {
                    Org = ctx.TenantId,
                    Id = ctx.StreamId["case:".Length..],
                    Event = ctx.EventId,
                    At = ctx.OccurredAt,
                    Passed = passed,
                    Reason = reason,
                    Detail = detail,
                    FailToPass = failToPass,
                    PassToPass = passToPass,
                    Seconds = seconds,
                },
                ctx.Transaction,
                cancellationToken: ctx.CancellationToken
            )
        );

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId["case:".Length..]);
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
