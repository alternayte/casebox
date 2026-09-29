using Casebox.Server.Features.Integrations;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.WorkItems;

// Links sessions to work items (docs/specs/integrations.md, Linking). It runs after a session
// arrives, after a pull request changes, and after a work item is imported. The strongest signal
// wins; a session that is already linked keeps its link unless a person moves it.
public sealed class Linker(NpgsqlDataSource db, IEventStore store, IntegrationStore integrations, DeedboxContext context, TimeProvider clock)
{
    private sealed record SessionRow(string Id, string? Repo, string? Branch, string? WorkItem, string? WorkItemId, DateTime StartedAt, DateTime? EndedAt);

    public async Task<WorkItemKeys> KeysAsync(CancellationToken ct)
    {
        var jira = await integrations.GetAsync<JiraSettings, JiraSecret>(context.TenantId, "jira", ct);
        return new WorkItemKeys(jira?.Config.Projects ?? []);
    }

    public async Task LinkSessionAsync(string sessionId, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var session = await connection.QuerySingleOrDefaultAsync<SessionRow>(new CommandDefinition(
            "SELECT id, repo, branch, work_item, work_item_id, started_at, ended_at FROM casebox.sessions WHERE org_id = @Org AND id = @Id",
            new { Org = context.TenantId, Id = sessionId }, cancellationToken: ct));
        if (session is null || session.WorkItemId is not null || session.Repo is null) return;
        var keys = await KeysAsync(ct);

        if (keys.FromExplicit(session.WorkItem, session.Repo) is { } explicitItem && await TryLinkAsync(explicitItem, session.Id, LinkSource.Explicit, Confidence.Explicit, ct))
            return;
        foreach (var item in keys.FromBranch(session.Branch, session.Repo))
            if (await TryLinkAsync(item, session.Id, LinkSource.Branch, Confidence.High, ct))
                return;

        if (session.Branch is not null)
        {
            var viaPr = await connection.QueryFirstOrDefaultAsync<string>(new CommandDefinition(
                "SELECT work_item_id FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND head_ref = @Branch AND work_item_id IS NOT NULL ORDER BY updated_at DESC",
                new { Org = context.TenantId, session.Repo, session.Branch }, cancellationToken: ct));
            if (viaPr is not null && await TryLinkAsync(viaPr, session.Id, LinkSource.SameBranch, Confidence.High, ct))
                return;
        }

        await SuggestAsync(connection, session, ct);
    }

    // Links every unlinked session on a branch, after its pull request linked to a work item.
    public async Task LinkBranchAsync(string repo, string branch, CancellationToken ct)
    {
        foreach (var id in await UnlinkedAsync("repo = @Repo AND branch = @Branch", new { Org = context.TenantId, Repo = repo, Branch = branch }, ct))
            await LinkSessionAsync(id, ct);
    }

    // Links sessions waiting for a work item that has just been imported.
    public async Task LinkWaitingAsync(CancellationToken ct)
    {
        foreach (var id in await UnlinkedAsync("(work_item IS NOT NULL OR branch IS NOT NULL) AND started_at > now() - interval '180 days'", new { Org = context.TenantId }, ct))
            await LinkSessionAsync(id, ct);
    }

    private async Task<IReadOnlyList<string>> UnlinkedAsync(string where, object parameters, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        return (await connection.QueryAsync<string>(new CommandDefinition(
            $"SELECT id FROM casebox.sessions WHERE org_id = @Org AND work_item_id IS NULL AND {where}", parameters, cancellationToken: ct))).ToList();
    }

    private async Task<bool> TryLinkAsync(string workItemId, string sessionId, LinkSource source, Confidence confidence, CancellationToken ct)
    {
        var (item, _) = await store.Load<WorkItem>(workItemId);
        if (!item.Exists) return false;
        var current = await CurrentSnapshotAsync(workItemId, ct);
        await store.Execute<WorkItem>(workItemId, w => WorkItemDecider.LinkSession(w, sessionId, source, confidence, current));
        return true;
    }

    // The latest snapshot, which the "work started" snapshot repeats.
    public async Task<Snapshot?> CurrentSnapshotAsync(string workItemId, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var payload = await connection.QuerySingleOrDefaultAsync<string>(new CommandDefinition(
            "SELECT snapshot::text FROM casebox.work_items WHERE org_id = @Org AND id = @Id",
            new { Org = context.TenantId, Id = workItemId }, cancellationToken: ct));
        return payload is null ? null : System.Text.Json.JsonSerializer.Deserialize<Snapshot>(payload, new System.Text.Json.JsonSerializerOptions(System.Text.Json.JsonSerializerDefaults.Web));
    }

    // Time and file overlap only: a suggestion for a person to confirm, never a link.
    private async Task SuggestAsync(NpgsqlConnection connection, SessionRow session, CancellationToken ct)
    {
        await connection.ExecuteAsync(new CommandDefinition(
            """
            INSERT INTO casebox.session_link_suggestions (org_id, session_id, work_item_id, reason, created_at)
            SELECT DISTINCT @Org, @Session, pr.work_item_id, 'overlap', @Now
            FROM casebox.pull_requests pr
            WHERE pr.org_id = @Org AND pr.repo = @Repo AND pr.work_item_id IS NOT NULL
              AND pr.updated_at BETWEEN @From AND @To
              AND EXISTS (
                SELECT 1 FROM casebox.session_events e, jsonb_array_elements_text(coalesce(e.tool->'files', '[]')) f
                WHERE e.org_id = @Org AND e.session_id = @Session
                  AND pr.snapshot->'files' @> to_jsonb(ARRAY[f]))
            ON CONFLICT DO NOTHING
            """,
            new
            {
                Org = context.TenantId,
                Session = session.Id,
                session.Repo,
                From = new DateTimeOffset(DateTime.SpecifyKind(session.StartedAt, DateTimeKind.Utc)).AddDays(-3),
                To = new DateTimeOffset(DateTime.SpecifyKind(session.EndedAt ?? session.StartedAt, DateTimeKind.Utc)).AddDays(3),
                Now = clock.GetUtcNow(),
            },
            cancellationToken: ct));
    }
}
