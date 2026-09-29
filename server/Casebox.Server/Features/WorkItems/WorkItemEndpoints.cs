using System.Text.Json;
using Casebox.Server.Features.Auth;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.WorkItems;

public static class WorkItemEndpoints
{
    public sealed record WorkItemSummary(string Id, string Provider, string Key, string? Repo, string Title, string? Type, string? Status, bool Resolved, int Sessions, int PullRequests, int Merged, DateTimeOffset UpdatedAt);

    public sealed record TimelineEntry(DateTimeOffset At, string Kind, JsonElement Detail);

    public sealed record Reassign(string WorkItem);

    public static void MapWorkItems(this RouteGroupBuilder api)
    {
        var items = api.MapGroup("/work-items").WithTags("Work").RequireAuthorization(Policies.Viewer);

        items.MapGet("/", async (HttpContext http, NpgsqlDataSource db, string? query, int? limit) =>
        {
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var rows = await connection.QueryAsync<Row>(new CommandDefinition(
                """
                SELECT id, provider, key, repo, title, type, status, resolved, sessions, pull_requests, merged, updated_at
                FROM casebox.work_items WHERE org_id = @Org AND (@Query::text IS NULL OR key ILIKE '%' || @Query || '%' OR title ILIKE '%' || @Query || '%')
                ORDER BY updated_at DESC LIMIT @Limit
                """,
                new { Org = http.User.OrgId(), Query = query, Limit = Math.Clamp(limit ?? 100, 1, 500) }, cancellationToken: http.RequestAborted));
            return Results.Ok(rows.Select(r => r.ToSummary()).ToList());
        });

        // Work item IDs hold '/' and '#', so the ID is a query parameter.
        items.MapGet("/timeline", async (string id, HttpContext http, NpgsqlDataSource db) =>
        {
            var workItem = id;
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var exists = await connection.ExecuteScalarAsync<bool>(new CommandDefinition(
                "SELECT EXISTS (SELECT 1 FROM casebox.work_items WHERE org_id = @Org AND id = @Id)", new { Org = http.User.OrgId(), Id = workItem }, cancellationToken: http.RequestAborted));
            if (!exists) return Results.NotFound();
            var rows = await connection.QueryAsync<(DateTime At, string Kind, string Detail)>(new CommandDefinition(
                "SELECT at, kind, detail::text FROM casebox.work_item_timeline WHERE org_id = @Org AND work_item_id = @Id ORDER BY at, event_id",
                new { Org = http.User.OrgId(), Id = workItem }, cancellationToken: http.RequestAborted));
            return Results.Ok(rows.Select(r => new TimelineEntry(new DateTimeOffset(DateTime.SpecifyKind(r.At, DateTimeKind.Utc)), r.Kind, JsonDocument.Parse(r.Detail).RootElement.Clone())).ToList());
        });

        // A person moves a session to another work item: session_reassigned on the old one, an
        // explicit link on the new one. Never an edit.
        api.MapPut("/sessions/{id}/work-item", async (string id, Reassign body, HttpContext http, NpgsqlDataSource db, IEventStore store, Linker linker) =>
        {
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var current = await connection.QuerySingleOrDefaultAsync<(string Id, string? WorkItemId)?>(new CommandDefinition(
                "SELECT id, work_item_id FROM casebox.sessions WHERE org_id = @Org AND id = @Id", new { Org = http.User.OrgId(), Id = id }, cancellationToken: http.RequestAborted));
            if (current is null) return Results.NotFound();
            var (target, _) = await store.Load<WorkItem>(body.WorkItem ?? "");
            if (!target.Exists) throw new NotFoundException("The work item does not exist.");
            if (current.Value.WorkItemId == body.WorkItem) return Results.NoContent();

            if (current.Value.WorkItemId is { } old)
                await store.Execute<WorkItem>(old, w => WorkItemDecider.Reassign(w, id, body.WorkItem!));
            var snapshot = await linker.CurrentSnapshotAsync(body.WorkItem!, http.RequestAborted);
            await store.Execute<WorkItem>(body.WorkItem!, w => WorkItemDecider.LinkSession(w, id, LinkSource.Explicit, Confidence.Explicit, snapshot));
            return Results.NoContent();
        }).WithTags("Work").RequireAuthorization(Policies.Member);
    }

    private sealed record Row(string Id, string Provider, string Key, string? Repo, string Title, string? Type, string? Status, bool Resolved, int Sessions, int PullRequests, int Merged, DateTime UpdatedAt)
    {
        public WorkItemSummary ToSummary() => new(Id, Provider, Key, Repo, Privacy.Masking.Mask(Title)!, Type, Status, Resolved, Sessions, PullRequests, Merged, new DateTimeOffset(DateTime.SpecifyKind(UpdatedAt, DateTimeKind.Utc)));
    }
}
