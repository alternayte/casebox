using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Patterns;

// The pattern board (docs/specs/self-evolution.md, API). A pattern shows only with k mapped people
// behind its corrections; quotes carry no author and the day only.
public static class PatternEndpoints
{
    public sealed record Phases(int InSession, int BeforeMerge, int AfterMerge);

    public sealed record Quote(string Ref, string Text, string Day);

    public sealed record ProposalRef(string Id, string Kind, string Status, string? PrUrl);

    public sealed record PatternView(
        string Id,
        string Workspace,
        string WentWrong,
        string? Label,
        string Prevention,
        string Path,
        string Title,
        string Summary,
        bool Advisory,
        string Status,
        string? Reason,
        int Corrections,
        int People,
        Phases Phases,
        DateTimeOffset DetectedAt,
        DateTimeOffset UpdatedAt,
        ProposalRef? LatestProposal
    );

    public sealed record PatternList(IReadOnlyList<PatternView> Patterns, int Hidden, int K);

    public sealed record CaseRef(string Id, string Status, string? Split);

    public sealed record PatternDetail(
        PatternView Pattern,
        IReadOnlyList<Quote> Quotes,
        IReadOnlyList<CaseRef> Cases,
        IReadOnlyList<ProposalRef> Proposals
    );

    public sealed record DismissBody(string? Reason);

    private sealed record Row(
        string Id,
        string Workspace,
        string WentWrong,
        string? Label,
        string Prevention,
        string Path,
        string Title,
        string Summary,
        bool Advisory,
        string Status,
        string? Reason,
        string Refs,
        DateTime DetectedAt,
        DateTime UpdatedAt
    );

    private sealed record Fact(
        string Ref,
        string Phase,
        DateTime At,
        string Person,
        bool PersonMapped,
        string Period,
        string? Text,
        string? LabelSource
    );

    public static void MapPatterns(this RouteGroupBuilder api)
    {
        var patterns = api.MapGroup("/patterns")
            .WithTags("Patterns")
            .RequireAuthorization(Policies.Viewer);

        patterns.MapGet(
            "/",
            async (string? workspace, HttpContext http, NpgsqlDataSource db, Solo solo) =>
            {
                var ct = http.RequestAborted;
                var k = await solo.ViewAsync(ct);
                await using var connection = await db.OpenConnectionAsync(ct);
                var rows = await RowsAsync(connection, http.User.OrgId(), null, workspace, ct);
                var views = new List<PatternView>();
                var hidden = 0;
                foreach (var row in rows)
                {
                    var facts = await FactsAsync(connection, http.User.OrgId(), row, ct);
                    if (!Meets(facts, k))
                    {
                        hidden++;
                        continue;
                    }
                    var proposals = await ProposalsAsync(connection, http.User.OrgId(), row.Id, ct);
                    views.Add(View(row, facts, proposals.FirstOrDefault(), k));
                }
                return Results.Ok(new PatternList(views, hidden, k.K));
            }
        );

        patterns.MapGet(
            "/{id}",
            async (string id, HttpContext http, NpgsqlDataSource db, Solo solo) =>
            {
                var ct = http.RequestAborted;
                var org = http.User.OrgId();
                var k = await solo.ViewAsync(ct);
                await using var connection = await db.OpenConnectionAsync(ct);
                var row = (await RowsAsync(connection, org, id, null, ct)).SingleOrDefault();
                if (row is null)
                    return Results.NotFound();
                var facts = await FactsAsync(connection, org, row, ct);
                if (!Meets(facts, k))
                    return Cbx.Problem(
                        StatusCodes.Status403Forbidden,
                        Cbx.BelowK,
                        $"Fewer than {k.K} people are behind this pattern, so it is not shown."
                    );
                var refs = JsonSerializer.Deserialize<string[]>(row.Refs)!;
                var cases = (
                    await connection.QueryAsync<CaseRef>(
                        new CommandDefinition(
                            "SELECT id, status, split FROM casebox.case_catalog WHERE org_id = @Org AND source = ANY(@Sources) ORDER BY id",
                            new
                            {
                                Org = org,
                                Sources = refs.Select(r => $"steering:{r}").ToArray(),
                            },
                            cancellationToken: ct
                        )
                    )
                ).ToList();
                var proposals = await ProposalsAsync(connection, org, row.Id, ct);
                return Results.Ok(
                    new PatternDetail(
                        View(row, facts, proposals.FirstOrDefault(), k),
                        Quotes(facts),
                        cases,
                        proposals
                    )
                );
            }
        );

        patterns
            .MapPost(
                "/{id}/acknowledgement",
                async (string id, HttpContext http, IEventStore store) =>
                {
                    await store.Execute<Pattern>(
                        Pattern.StreamId(id),
                        p => PatternDecider.Acknowledge(p, http.User.Actor()!),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        patterns
            .MapPost(
                "/{id}/dismissal",
                async (string id, DismissBody body, HttpContext http, IEventStore store) =>
                {
                    await store.Execute<Pattern>(
                        Pattern.StreamId(id),
                        p => PatternDecider.Dismiss(p, http.User.Actor()!, body.Reason ?? ""),
                        http.RequestAborted
                    );
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);
    }

    public sealed record Evidence(
        string Title,
        string Summary,
        int Corrections,
        int People,
        bool MeetsK,
        IReadOnlyList<Quote> Quotes,
        IReadOnlyList<CaseRef> Cases
    );

    // A pattern's evidence for its pull request: counts and quotes only when k people are behind it.
    public static async Task<Evidence?> EvidenceAsync(
        System.Data.Common.DbConnection connection,
        string org,
        string id,
        KView k,
        CancellationToken ct
    )
    {
        var row = (await RowsAsync(connection, org, id, null, ct)).SingleOrDefault();
        if (row is null)
            return null;
        var facts = await FactsAsync(connection, org, row, ct);
        var refs = JsonSerializer.Deserialize<string[]>(row.Refs)!;
        var cases = (
            await connection.QueryAsync<CaseRef>(
                new CommandDefinition(
                    "SELECT id, status, split FROM casebox.case_catalog WHERE org_id = @Org AND source = ANY(@Sources) ORDER BY id",
                    new { Org = org, Sources = refs.Select(r => $"steering:{r}").ToArray() },
                    cancellationToken: ct
                )
            )
        ).ToList();
        var meets = Meets(facts, k);
        return new Evidence(
            row.Title,
            row.Summary,
            meets ? facts.Count : 0,
            meets
                ? KRule.People(facts.Select(f => new Person(f.Person, f.PersonMapped, f.Period)), k)
                : 0,
            meets,
            meets ? Quotes(facts) : [],
            cases
        );
    }

    private static bool Meets(IReadOnlyList<Fact> facts, KView k) =>
        KRule.Meets(facts.Select(f => new Person(f.Person, f.PersonMapped, f.Period)), k);

    private static async Task<List<Row>> RowsAsync(
        System.Data.Common.DbConnection connection,
        string org,
        string? id,
        string? workspace,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<Row>(
                new CommandDefinition(
                    """
                    SELECT id, workspace, went_wrong, label, prevention, path, title, summary, advisory, status, reason, refs::text AS refs, detected_at, updated_at
                    FROM casebox.patterns
                    WHERE org_id = @Org AND (@Id::text IS NULL OR id = @Id) AND (@Workspace::text IS NULL OR workspace = @Workspace)
                    ORDER BY status = 'dismissed', jsonb_array_length(refs) DESC, id
                    """,
                    new
                    {
                        Org = org,
                        Id = id,
                        Workspace = workspace,
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();

    // The pattern's corrections that still exist: an erased person's are gone.
    private static async Task<List<Fact>> FactsAsync(
        System.Data.Common.DbConnection connection,
        string org,
        Row row,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<Fact>(
                new CommandDefinition(
                    """
                    SELECT ref, phase, at, person, person_mapped, period, text, label_source FROM casebox.steering_facts
                    WHERE org_id = @Org AND ref = ANY(@Refs)
                    """,
                    new { Org = org, Refs = JsonSerializer.Deserialize<string[]>(row.Refs) },
                    cancellationToken: ct
                )
            )
        ).ToList();

    public static async Task<List<ProposalRef>> ProposalsAsync(
        System.Data.Common.DbConnection connection,
        string org,
        string pattern,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<ProposalRef>(
                new CommandDefinition(
                    "SELECT id, kind, status, pr_url FROM casebox.proposals WHERE org_id = @Org AND pattern = @Pattern ORDER BY created_at DESC",
                    new { Org = org, Pattern = pattern },
                    cancellationToken: ct
                )
            )
        ).ToList();

    private static PatternView View(
        Row r,
        IReadOnlyList<Fact> facts,
        ProposalRef? latest,
        KView k
    ) =>
        new(
            r.Id,
            r.Workspace,
            r.WentWrong,
            r.Label,
            r.Prevention,
            r.Path,
            r.Title,
            r.Summary,
            r.Advisory,
            r.Status,
            r.Reason,
            facts.Count,
            KRule.People(facts.Select(f => new Person(f.Person, f.PersonMapped, f.Period)), k),
            new Phases(
                facts.Count(f => f.Phase == "in_session"),
                facts.Count(f => f.Phase == "before_merge"),
                facts.Count(f => f.Phase == "after_merge")
            ),
            Utc(r.DetectedAt),
            Utc(r.UpdatedAt),
            latest
        );

    // Up to three quotes from different people: masked, without authors, dated by day.
    private static IReadOnlyList<Quote> Quotes(IReadOnlyList<Fact> facts) =>
        facts
            .Where(f => !string.IsNullOrWhiteSpace(f.Text))
            .OrderByDescending(f => f.LabelSource == "human")
            .ThenByDescending(f => f.At)
            .DistinctBy(f => f.Person)
            .Take(3)
            .Select(f =>
            {
                var text = Masking.Mask(f.Text)!;
                return new Quote(
                    f.Ref,
                    text.Length > 600 ? text[..600] : text,
                    Utc(f.At)
                        .ToString("yyyy-MM-dd", System.Globalization.CultureInfo.InvariantCulture)
                );
            })
            .ToList();

    private static DateTimeOffset Utc(DateTime at) =>
        new(DateTime.SpecifyKind(at, DateTimeKind.Utc));
}
