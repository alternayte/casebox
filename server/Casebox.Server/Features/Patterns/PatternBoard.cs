using System.Text.Json;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Patterns;

// The pattern board: every pattern with its group, its corrections (by opaque ref) and its status.
// Inline, so a request reads its own writes.
public sealed class PatternBoard : Projection
{
    public const string Name = "pattern_board";

    public PatternBoard()
    {
        On<PatternEvents.Detected>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.patterns (org_id, id, workspace, went_wrong, label, prevention, path, title, summary, refs, advisory, status, detected_at, updated_at)
                    VALUES (@Org, @Id, @Workspace, @WentWrong, @Label, @Prevention, @Path, @Title, @Summary, @Refs::jsonb, @Advisory, 'open', @At, @At)
                    ON CONFLICT DO NOTHING
                    """,
                    new
                    {
                        e.Workspace,
                        e.WentWrong,
                        e.Label,
                        e.Prevention,
                        e.Path,
                        e.Title,
                        e.Summary,
                        Refs = JsonSerializer.Serialize(e.Refs),
                        e.Advisory,
                    }
                )
        );
        On<PatternEvents.CorrectionsAdded>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.patterns SET refs = refs || @Refs::jsonb, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Refs = JsonSerializer.Serialize(e.Refs) }
                )
        );
        On<PatternEvents.Acknowledged>((_, ctx) => Status(ctx, "acknowledged", null));
        On<PatternEvents.Dismissed>((e, ctx) => Status(ctx, "dismissed", e.Reason));
        On<PatternEvents.Resolved>((_, ctx) => Status(ctx, "resolved", null));
        On<PatternEvents.Reopened>((e, ctx) => Status(ctx, "open", e.Reason));
        On<PatternEvents.AdvisoryNoted>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.patterns SET advisory = true, advisory_note = @Note, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Note }
                )
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.patterns;",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task Status(ProjectionContext ctx, string status, string? reason) =>
        Exec(
            ctx,
            "UPDATE casebox.patterns SET status = @Status, reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
            new { Status = status, Reason = reason }
        );

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId["pattern:".Length..]);
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
