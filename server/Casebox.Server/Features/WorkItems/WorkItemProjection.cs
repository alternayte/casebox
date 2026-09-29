using System.Text.Json;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.WorkItems;

// Inline, so the Work page and the linker read their own writes: the work item list, its
// timeline, and each session's current link.
public sealed class WorkItemProjection : Projection
{
    public const string Name = "work_items";

    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        Converters = { Infrastructure.CaseboxStreams.Enums },
    };

    public WorkItemProjection()
    {
        On<WorkItemEvents.Imported>(
            (e, ctx) =>
                Exec(
                    ctx,
                    """
                    INSERT INTO casebox.work_items (org_id, id, provider, key, repo, title, created_at, updated_at)
                    VALUES (@Org, @Id, @Provider, @Key, @Repo, '', @Created, @At) ON CONFLICT (org_id, id) DO NOTHING
                    """,
                    new
                    {
                        e.Provider,
                        e.Key,
                        e.Repo,
                        Created = e.CreatedAt,
                    }
                )
        );

        On<WorkItemEvents.SnapshotCaptured>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    "UPDATE casebox.work_items SET title = @Title, type = @Type, status = @Status, resolved = @Resolved, snapshot = @Snapshot::jsonb, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new
                    {
                        e.Snapshot.Title,
                        e.Snapshot.Type,
                        e.Snapshot.Status,
                        e.Snapshot.Resolved,
                        Snapshot = JsonSerializer.Serialize(e.Snapshot, Json),
                    }
                );
                await Timeline(
                    ctx,
                    "snapshot",
                    new
                    {
                        reason = e.Reason,
                        e.Snapshot.Status,
                        e.Snapshot.Resolved,
                    }
                );
            }
        );

        On<WorkItemEvents.SessionLinked>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    """
                    UPDATE casebox.sessions SET work_item_id = @Id, link_source = @Source, link_confidence = @Confidence WHERE org_id = @Org AND id = @Session;
                    UPDATE casebox.work_items SET sessions = (SELECT count(*) FROM casebox.sessions WHERE org_id = @Org AND work_item_id = @Id), updated_at = @At WHERE org_id = @Org AND id = @Id;
                    DELETE FROM casebox.session_link_suggestions WHERE org_id = @Org AND session_id = @Session;
                    """,
                    new
                    {
                        Session = e.SessionId,
                        Source = Enum(e.Source),
                        Confidence = Enum(e.Confidence),
                    }
                );
                await Timeline(
                    ctx,
                    "session_linked",
                    new
                    {
                        sessionId = e.SessionId,
                        source = e.Source,
                        confidence = e.Confidence,
                    }
                );
            }
        );

        On<WorkItemEvents.SessionReassigned>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    """
                    UPDATE casebox.sessions SET work_item_id = NULL, link_source = NULL, link_confidence = NULL WHERE org_id = @Org AND id = @Session AND work_item_id = @Id;
                    UPDATE casebox.work_items SET sessions = (SELECT count(*) FROM casebox.sessions WHERE org_id = @Org AND work_item_id = @Id), updated_at = @At WHERE org_id = @Org AND id = @Id;
                    """,
                    new { Session = e.SessionId }
                );
                await Timeline(
                    ctx,
                    "session_reassigned",
                    new { sessionId = e.SessionId, to = e.ToWorkItem }
                );
            }
        );

        On<WorkItemEvents.PrLinked>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    """
                    UPDATE casebox.pull_requests SET work_item_id = @Id WHERE org_id = @Org AND repo = @Repo AND number = @Number;
                    UPDATE casebox.work_items SET pull_requests = pull_requests + 1, updated_at = @At WHERE org_id = @Org AND id = @Id;
                    """,
                    new { e.Repo, e.Number }
                );
                await Timeline(
                    ctx,
                    "pr_linked",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        source = e.Source,
                        confidence = e.Confidence,
                    }
                );
            }
        );

        On<WorkItemEvents.Merged>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    "UPDATE casebox.work_items SET merged = merged + 1, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    null
                );
                await Timeline(
                    ctx,
                    "merged",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        sha = e.Sha,
                    },
                    e.At
                );
            }
        );

        On<WorkItemEvents.ReviewObserved>(
            (e, ctx) =>
                Timeline(
                    ctx,
                    "review",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        state = e.State,
                        comments = e.Comments,
                    },
                    e.At
                )
        );
        On<WorkItemEvents.CiObserved>(
            (e, ctx) =>
                Timeline(
                    ctx,
                    "ci",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        check = e.Check,
                        conclusion = e.Conclusion,
                    },
                    e.At
                )
        );
        On<WorkItemEvents.RevertObserved>(
            (e, ctx) =>
                Timeline(
                    ctx,
                    "revert",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        by = e.RevertedBy,
                    },
                    e.At
                )
        );
        On<WorkItemEvents.FixObserved>(
            (e, ctx) =>
                Timeline(
                    ctx,
                    "fix",
                    new
                    {
                        repo = e.Repo,
                        number = e.Number,
                        by = e.FixedBy,
                        lines = e.Lines,
                        source = e.Source,
                    },
                    e.At
                )
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                """
                DELETE FROM casebox.work_item_timeline;
                DELETE FROM casebox.work_items;
                UPDATE casebox.sessions SET work_item_id = NULL, link_source = NULL, link_confidence = NULL;
                UPDATE casebox.pull_requests SET work_item_id = NULL;
                """,
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static string Enum<T>(T value)
        where T : struct, System.Enum => JsonSerializer.Serialize(value, Json).Trim('"');

    private static Task Exec(ProjectionContext ctx, string sql, object? values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId);
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

    private static Task Timeline(
        ProjectionContext ctx,
        string kind,
        object detail,
        DateTimeOffset? at = null
    ) =>
        ctx.Connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.work_item_timeline (org_id, work_item_id, event_id, at, kind, detail)
                VALUES (@Org, @Id, @Event, @At, @Kind, @Detail::jsonb) ON CONFLICT DO NOTHING
                """,
                new
                {
                    Org = ctx.TenantId,
                    Id = ctx.StreamId,
                    Event = ctx.EventId,
                    At = at ?? ctx.OccurredAt,
                    Kind = kind,
                    Detail = JsonSerializer.Serialize(detail, Json),
                },
                ctx.Transaction,
                cancellationToken: ctx.CancellationToken
            )
        );
}
