using System.Text.Json;
using Casebox.Server.Infrastructure;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Proposals;

// The proposal board: every proposal with its change, its status and its outcome. Inline, so a
// request reads its own writes.
public sealed class ProposalBoard : Projection
{
    public const string Name = "proposal_board";

    public static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        Converters = { CaseboxStreams.Enums },
    };

    public ProposalBoard()
    {
        On<ProposalEvents.Drafted>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.proposals (org_id, id, workspace, pattern, repo, kind, title, rationale, edits, preview, note,
                                                   content_hash, base_commit, status, created_at, updated_at)
                    VALUES (@Org, @Id, @Workspace, @Pattern, @Repo, @Kind, @Title, @Rationale, @Edits::jsonb, @Preview::jsonb, @Note::jsonb,
                            @ContentHash, @BaseCommit, 'open', @At, @At)
                    ON CONFLICT DO NOTHING
                    """,
                    new
                    {
                        e.Workspace,
                        e.Pattern,
                        e.Repo,
                        Kind = KindName(e.Kind),
                        e.Title,
                        e.Rationale,
                        Edits = JsonSerializer.Serialize(e.Edits, Json),
                        Preview = JsonSerializer.Serialize(e.Preview, Json),
                        Note = e.Note is null ? null : JsonSerializer.Serialize(e.Note, Json),
                        e.ContentHash,
                        e.BaseCommit,
                    }
                )
        );
        On<ProposalEvents.Approved>(
            (_, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'approved', updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { }
                )
        );
        On<ProposalEvents.Rejected>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'rejected', reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Reason }
                )
        );
        On<ProposalEvents.Applied>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    UPDATE casebox.proposals SET status = 'applied', applied_mode = @Mode, applied_at = coalesce(applied_at, @AppliedAt), updated_at = @At
                    WHERE org_id = @Org AND id = @Id
                    """,
                    new
                    {
                        Mode = e.Mode == ApplyMode.Commit ? "commit" : "private",
                        AppliedAt = e.At,
                    }
                )
        );
        On<ProposalEvents.OutcomeObserved>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET outcome = @Outcome::jsonb, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Outcome = JsonSerializer.Serialize(e, Json) }
                )
        );
    }

    public static string KindName(ProposalKind kind) =>
        kind switch
        {
            ProposalKind.HarnessEdit => "harness_edit",
            ProposalKind.Skill => "skill",
            ProposalKind.Mcp => "mcp",
            _ => "code_note",
        };

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.proposals;",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId["proposal:".Length..]);
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
