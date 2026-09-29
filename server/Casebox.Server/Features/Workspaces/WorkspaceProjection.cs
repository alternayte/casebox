using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Workspaces;

// Inline, so an API call right after a command reads its own write.
public sealed class WorkspaceProjection : Projection
{
    public const string Name = "workspaces";

    public WorkspaceProjection()
    {
        On<WorkspaceEvents.Created>(
            (e, ctx) =>
                ctx.Connection.ExecuteAsync(
                    new CommandDefinition(
                        """
                        INSERT INTO casebox.workspaces (org_id, id, name, repos, recipe_status, updated_at)
                        VALUES (@Org, @Id, @Name, '[]', 'none', @At)
                        """,
                        new
                        {
                            Org = ctx.TenantId,
                            Id = ctx.StreamId,
                            e.Name,
                            At = ctx.OccurredAt,
                        },
                        ctx.Transaction,
                        cancellationToken: ctx.CancellationToken
                    )
                )
        );

        On<WorkspaceEvents.RepoAdded>(
            (e, ctx) =>
                Update(
                    ctx,
                    "repos = (SELECT jsonb_agg(r ORDER BY r) FROM (SELECT DISTINCT jsonb_array_elements_text(repos || to_jsonb(@Repo::text)) AS r) s)",
                    new { e.Repo }
                )
        );

        On<WorkspaceEvents.RepoRemoved>(
            (e, ctx) =>
                Update(
                    ctx,
                    "repos = COALESCE((SELECT jsonb_agg(r ORDER BY r) FROM jsonb_array_elements_text(repos) r WHERE r <> @Repo), '[]')",
                    new { e.Repo }
                )
        );

        On<WorkspaceEvents.HarnessConfigured>(
            (e, ctx) => Update(ctx, "shared_harness = @Shared", new { e.Shared })
        );

        On<WorkspaceEvents.RecipeProposed>(
            (_, ctx) => Update(ctx, "recipe_status = 'proposed'", null)
        );
        On<WorkspaceEvents.RecipeValidated>(
            (e, ctx) =>
                Update(
                    ctx,
                    "recipe_status = @Status",
                    new { Status = e.Passed ? "validated" : "validation_failed" }
                )
        );
        On<WorkspaceEvents.RecipeConfirmed>(
            (_, ctx) => Update(ctx, "recipe_status = 'confirmed'", null)
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.workspaces",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task Update(ProjectionContext ctx, string set, object? values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId);
        parameters.Add("At", ctx.OccurredAt);
        return ctx.Connection.ExecuteAsync(
            new CommandDefinition(
                $"UPDATE casebox.workspaces SET {set}, updated_at = @At WHERE org_id = @Org AND id = @Id",
                parameters,
                ctx.Transaction,
                cancellationToken: ctx.CancellationToken
            )
        );
    }
}
