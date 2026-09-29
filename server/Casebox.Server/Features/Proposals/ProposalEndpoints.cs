using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

// Proposals (docs/specs/simple-evolution.md): a person reads each one with its evidence, approves or
// rejects it, and `casebox apply` reports when it landed. A proposal shows only while its pattern
// has k people behind it, because it carries the pattern's quotes.
public static class ProposalEndpoints
{
    public sealed record ProposalSummary(
        string Id,
        string Workspace,
        string Pattern,
        string PatternTitle,
        string Repo,
        string Kind,
        string Title,
        string Status,
        DateTimeOffset CreatedAt,
        DateTimeOffset? AppliedAt,
        string? AppliedMode,
        JsonElement? Outcome
    );

    public sealed record ProposalList(IReadOnlyList<ProposalSummary> Proposals, int Hidden);

    public sealed record ProposalDetail(
        ProposalSummary Proposal,
        string Rationale,
        string? Reason,
        JsonElement Edits,
        JsonElement Preview,
        JsonElement? Note,
        string BaseCommit,
        PatternEndpoints.Evidence Evidence
    );

    public sealed record RejectBody(string? Reason);

    public sealed record AppliedBody(ApplyMode Mode);

    private sealed record Row(
        string Id,
        string Workspace,
        string Pattern,
        string PatternTitle,
        string Repo,
        string Kind,
        string Title,
        string Rationale,
        string Edits,
        string Preview,
        string? Note,
        string BaseCommit,
        string Status,
        string? Reason,
        DateTime CreatedAt,
        DateTime? AppliedAt,
        string? AppliedMode,
        string? Outcome
    );

    public static void MapProposals(this RouteGroupBuilder api)
    {
        var proposals = api.MapGroup("/proposals").WithTags("Proposals");

        proposals
            .MapGet(
                "/",
                async (
                    string? workspace,
                    string? status,
                    HttpContext http,
                    NpgsqlDataSource db,
                    Solo solo
                ) =>
                {
                    var ct = http.RequestAborted;
                    var org = http.User.OrgId();
                    var k = await solo.ViewAsync(ct);
                    await using var connection = await db.OpenConnectionAsync(ct);
                    var shown = new List<ProposalSummary>();
                    var hidden = 0;
                    foreach (
                        var row in await RowsAsync(connection, org, null, workspace, status, ct)
                    )
                    {
                        var evidence = await PatternEndpoints.EvidenceAsync(
                            connection,
                            org,
                            row.Pattern,
                            k,
                            ct
                        );
                        if (evidence?.MeetsK != true)
                        {
                            hidden++;
                            continue;
                        }
                        shown.Add(Summary(row));
                    }
                    return Results.Ok(new ProposalList(shown, hidden));
                }
            )
            .RequireAuthorization(Policies.Viewer);

        proposals
            .MapGet(
                "/{id}",
                async (string id, HttpContext http, NpgsqlDataSource db, Solo solo) =>
                {
                    var ct = http.RequestAborted;
                    var org = http.User.OrgId();
                    var k = await solo.ViewAsync(ct);
                    await using var connection = await db.OpenConnectionAsync(ct);
                    var row = (
                        await RowsAsync(connection, org, id, null, null, ct)
                    ).SingleOrDefault();
                    if (row is null)
                        return Results.NotFound();
                    var evidence = await PatternEndpoints.EvidenceAsync(
                        connection,
                        org,
                        row.Pattern,
                        k,
                        ct
                    );
                    if (evidence?.MeetsK != true)
                        return Cbx.Problem(
                            StatusCodes.Status403Forbidden,
                            Cbx.BelowK,
                            $"Fewer than {k.K} people are behind this proposal's pattern, so it is not shown."
                        );
                    return Results.Ok(
                        new ProposalDetail(
                            Summary(row),
                            row.Rationale,
                            row.Reason,
                            Parse(row.Edits),
                            Parse(row.Preview),
                            row.Note is null ? null : Parse(row.Note),
                            row.BaseCommit,
                            evidence
                        )
                    );
                }
            )
            .RequireAuthorization(Policies.Viewer);

        proposals
            .MapPost(
                "/{id}/approval",
                async (string id, HttpContext http, IEventStore store) =>
                {
                    await store.Execute<Proposal>(
                        Proposal.StreamId(id),
                        p => ProposalDecider.Approve(p, http.User.Actor()!),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        proposals
            .MapPost(
                "/{id}/rejection",
                async (string id, RejectBody body, HttpContext http, IEventStore store) =>
                {
                    await store.Execute<Proposal>(
                        Proposal.StreamId(id),
                        p => ProposalDecider.Reject(p, body.Reason ?? "", http.User.Actor()!),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        // `casebox apply` reports that the change is in the person's working tree.
        proposals
            .MapPost(
                "/{id}/applied",
                async (
                    string id,
                    AppliedBody body,
                    HttpContext http,
                    IEventStore store,
                    TimeProvider clock
                ) =>
                {
                    await store.Execute<Proposal>(
                        Proposal.StreamId(id),
                        p =>
                            ProposalDecider.Apply(
                                p,
                                body.Mode,
                                http.User.Actor()!,
                                clock.GetUtcNow()
                            ),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);
    }

    private static async Task<List<Row>> RowsAsync(
        NpgsqlConnection connection,
        string org,
        string? id,
        string? workspace,
        string? status,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<Row>(
                new CommandDefinition(
                    """
                    SELECT x.id, x.workspace, x.pattern, coalesce(p.title, '') AS pattern_title, x.repo, x.kind, x.title, x.rationale,
                           x.edits::text AS edits, x.preview::text AS preview, x.note::text AS note, x.base_commit, x.status, x.reason,
                           x.created_at, x.applied_at, x.applied_mode, x.outcome::text AS outcome
                    FROM casebox.proposals x
                    LEFT JOIN casebox.patterns p ON p.org_id = x.org_id AND p.id = x.pattern
                    WHERE x.org_id = @Org AND (@Id::text IS NULL OR x.id = @Id) AND (@Workspace::text IS NULL OR x.workspace = @Workspace)
                      AND (@Status::text IS NULL OR x.status = @Status)
                    ORDER BY x.status = 'open' DESC, x.status = 'approved' DESC, x.created_at DESC
                    """,
                    new
                    {
                        Org = org,
                        Id = id,
                        Workspace = workspace,
                        Status = status,
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();

    private static ProposalSummary Summary(Row r) =>
        new(
            r.Id,
            r.Workspace,
            r.Pattern,
            r.PatternTitle,
            r.Repo,
            r.Kind,
            r.Title,
            r.Status,
            Utc(r.CreatedAt),
            r.AppliedAt is { } a ? Utc(a) : null,
            r.AppliedMode,
            r.Outcome is null ? null : Parse(r.Outcome)
        );

    private static JsonElement Parse(string json) => JsonDocument.Parse(json).RootElement.Clone();

    private static DateTimeOffset Utc(DateTime at) =>
        new(DateTime.SpecifyKind(at, DateTimeKind.Utc));
}
