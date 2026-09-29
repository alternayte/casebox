using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

public static class ProposalJobs
{
    public const string Search = "propose.search";
    public const string Diet = "propose.diet";

    // The dev batch: at most this many cases per score.
    public const int BatchSize = 8;

    // A rejected change is not proposed again for this long (SDD section 9, After the merge).
    public static readonly TimeSpan RejectionMemory = TimeSpan.FromDays(90);
}

// A candidate as the worker drafts it: its edits, the blob of its files, and why.
public sealed record DraftedCandidate(
    IReadOnlyList<Edit> Edits,
    string Overrides,
    string Rationale
);

public sealed record DraftAnswer(string BaseCommit, IReadOnlyList<DraftedCandidate> Candidates);

// Turns the worker's candidates into a drafted proposal: rejected changes of the last 90 days are
// dropped, and the dev batch is fixed for every candidate (docs/specs/self-evolution.md, Search).
public sealed class DraftResultHandler(TimeProvider clock) : IJobResultHandler
{
    public string Kind => ProposalJobs.Search;

    public Task HandleAsync(JobResult result, CancellationToken ct) =>
        DraftAsync(result, clock, ct);

    public static async Task DraftAsync(JobResult result, TimeProvider clock, CancellationToken ct)
    {
        var payload = result.Job.Payload;
        var answer =
            result.Result.Deserialize<DraftAnswer>(EvaluationResults.Json)
            ?? throw new DomainException("The result is empty.");
        var request = payload
            .GetProperty("request")
            .Deserialize<ProposerRequest>(EvaluationResults.Json)!;
        var pattern = payload.TryGetProperty("pattern", out var pt) ? pt.GetString() : null;
        var kind = pattern is null ? ProposalKind.Removal : ProposalKind.Edit;
        var connection = result.Transaction.Connection!;

        var rejected = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    """
                    SELECT c.content_hash FROM casebox.proposal_candidates c
                    JOIN casebox.proposals p ON p.org_id = c.org_id AND p.id = c.proposal_id
                    WHERE c.org_id = @Org AND p.status = 'rejected' AND p.updated_at >= @Since
                    """,
                    new
                    {
                        Org = result.OrgId,
                        Since = clock.GetUtcNow() - ProposalJobs.RejectionMemory,
                    },
                    result.Transaction,
                    cancellationToken: ct
                )
            )
        ).ToHashSet();

        var candidates = answer
            .Candidates.Select(c => (c, Hash: ProposalSteps.ContentHash(c.Edits)))
            .Where(x => !rejected.Contains(x.Hash))
            .DistinctBy(x => x.Hash)
            .Take(ProposalDecider.MaxCandidates)
            .ToList();

        // A removal job answers many candidates: each is its own proposal.
        var groups =
            kind == ProposalKind.Removal ? candidates.Select(x => new[] { x }.ToList()).ToList()
            : candidates.Count == 0 ? []
            : [candidates];
        var ids = payload
            .GetProperty("proposals")
            .EnumerateArray()
            .Select(p => p.GetString()!)
            .ToList();
        for (var i = 0; i < groups.Count && i < ids.Count; i++)
        {
            var list = groups[i];
            var batch = await BatchAsync(
                connection,
                result,
                request,
                pattern,
                ids[i],
                list.Count,
                ct
            );
            var drafted = new ProposalEvents.Drafted(
                pattern,
                request.Workspace,
                kind,
                payload.GetProperty("repo").GetString()!,
                answer.BaseCommit,
                [
                    .. list.Select(
                        (x, index) =>
                            new Candidate(index, x.c.Edits, x.c.Overrides, x.Hash, x.c.Rationale)
                    ),
                ],
                request.Spec,
                request.Prices ?? new Dictionary<string, Price>(),
                request.Repeats ?? 3,
                request.BudgetRuns ?? 40,
                batch,
                payload.TryGetProperty("ciRun", out var ciRun) ? ciRun.GetString() : null
            );
            await result.Store.Execute<Proposal>(
                Proposal.StreamId(ids[i]),
                p => ProposalDecider.Draft(p, drafted),
                ct
            );
        }
    }

    // The pattern's steering cases first, then a random sample of other dev cases seeded by the
    // proposal, all with a baseline score, sized so every candidate and one merge fit the budget.
    private static async Task<IReadOnlyList<string>> BatchAsync(
        System.Data.Common.DbConnection connection,
        JobResult result,
        ProposerRequest request,
        string? pattern,
        string proposal,
        int candidates,
        CancellationToken ct
    )
    {
        var size = Math.Max(
            1,
            Math.Min(ProposalJobs.BatchSize, (request.BudgetRuns ?? 40) / (candidates + 1))
        );
        var scored = await BaselineCache.ScoresAsync(
            connection,
            result.OrgId,
            request.Workspace,
            HarnessSpec.Key(request.Spec),
            null,
            ct
        );
        var own = new HashSet<string>();
        if (pattern is not null)
        {
            var (p, _) = await result.Store.Load<Pattern>(Pattern.StreamId(pattern), ct);
            own = (
                await connection.QueryAsync<string>(
                    new CommandDefinition(
                        "SELECT id FROM casebox.case_catalog WHERE org_id = @Org AND source = ANY(@Sources)",
                        new
                        {
                            Org = result.OrgId,
                            Sources = p.Refs.Select(r => $"steering:{r}").ToArray(),
                        },
                        result.Transaction,
                        cancellationToken: ct
                    )
                )
            ).ToHashSet();
        }
        var rng = new Statistics.Rng(Statistics.SeedOf(proposal));
        var others = scored
            .Keys.Where(k => !own.Contains(k))
            .Order(StringComparer.Ordinal)
            .ToList();
        for (var i = others.Count - 1; i > 0; i--)
        {
            var j = rng.NextInt(i + 1);
            (others[i], others[j]) = (others[j], others[i]);
        }
        return
        [
            .. scored
                .Keys.Where(own.Contains)
                .Order(StringComparer.Ordinal)
                .Concat(others)
                .Take(size),
        ];
    }
}

public sealed class DietResultHandler(TimeProvider clock) : IJobResultHandler
{
    public string Kind => ProposalJobs.Diet;

    public Task HandleAsync(JobResult result, CancellationToken ct) =>
        DraftResultHandler.DraftAsync(result, clock, ct);
}

// What a proposer run asks for: casebox.yml's baseline, prices and repeats.
public sealed record ProposerRequest(
    string Workspace,
    HarnessSpec Spec,
    IReadOnlyDictionary<string, Price>? Prices,
    int? Repeats,
    int? BudgetRuns,
    string? Pattern,
    string? Repo
);

public static class ProposerWorkerEndpoints
{
    public sealed record Evidence(
        object Pattern,
        IReadOnlyList<SteeringWorkerEndpoints.Window> Windows,
        IReadOnlyList<object> Cases,
        IReadOnlyList<object> Rejections
    );

    // What the proposer may read: the pattern's windows (masked), its dev cases, and the rejections of
    // the last 90 days for the pattern or its workspace's harness files, with their reasons. Never a
    // held-out case.
    public static void MapProposerWorker(this RouteGroupBuilder worker)
    {
        worker
            .MapGet(
                "/patterns/{id}/evidence",
                async (
                    string id,
                    HttpContext http,
                    NpgsqlDataSource db,
                    IEventStore store,
                    SteeringWindows windows,
                    TimeProvider clock
                ) =>
                {
                    var ct = http.RequestAborted;
                    var org = http.User.OrgId();
                    var (p, _) = await store.Load<Pattern>(Pattern.StreamId(id), ct);
                    if (!p.Exists)
                        return Results.NotFound();
                    await using var connection = await db.OpenConnectionAsync(ct);
                    // A ci token's inline worker reads only the pattern of a search job of its own run.
                    if (
                        http.User.IsCiToken()
                        && !await connection.ExecuteScalarAsync<bool>(
                            new CommandDefinition(
                                """
                                SELECT EXISTS (
                                    SELECT 1 FROM casebox.jobs j JOIN casebox.ci_runs c ON c.org_id = j.org_id AND c.id = j.payload->>'ciRun'
                                    WHERE j.org_id = @Org AND j.kind = @Kind AND j.payload->>'pattern' = @Pattern AND j.status = 'leased' AND c.created_by = @Token)
                                """,
                                new
                                {
                                    Org = org,
                                    Kind = ProposalJobs.Search,
                                    Pattern = id,
                                    Token = $"token:{http.User.TokenId()}",
                                },
                                cancellationToken: ct
                            )
                        )
                    )
                        return Results.NotFound();
                    var facts = await connection.QueryAsync<(string Stream, string Intervention)>(
                        new CommandDefinition(
                            "SELECT stream_id, intervention_id FROM casebox.steering_facts WHERE org_id = @Org AND ref = ANY(@Refs) ORDER BY at DESC LIMIT 30",
                            new { Org = org, Refs = p.Refs.ToArray() },
                            cancellationToken: ct
                        )
                    );
                    var found = new List<SteeringWorkerEndpoints.Window>();
                    foreach (var g in facts.GroupBy(f => f.Stream))
                        found.AddRange(
                            await windows.ForAsync(g.Key, [.. g.Select(f => f.Intervention)], ct)
                        );
                    var cases = (
                        await connection.QueryAsync<(string Id, string? Instruction)>(
                            new CommandDefinition(
                                "SELECT id, instruction FROM casebox.case_catalog WHERE org_id = @Org AND source = ANY(@Sources) AND split = 'dev' AND status = 'approved'",
                                new
                                {
                                    Org = org,
                                    Sources = p.Refs.Select(r => $"steering:{r}").ToArray(),
                                },
                                cancellationToken: ct
                            )
                        )
                    )
                        .Select(c =>
                            (object)new { c.Id, Instruction = Masking.Mask(c.Instruction) }
                        )
                        .ToList();
                    var rejections = (
                        await connection.QueryAsync<(string Edits, string? Reason)>(
                            new CommandDefinition(
                                """
                                SELECT c.edits::text, p.reason FROM casebox.proposal_candidates c
                                JOIN casebox.proposals p ON p.org_id = c.org_id AND p.id = c.proposal_id
                                WHERE c.org_id = @Org AND p.status = 'rejected' AND p.updated_at >= @Since
                                  AND (p.pattern = @Pattern OR p.workspace = @Workspace)
                                """,
                                new
                                {
                                    Org = org,
                                    Since = clock.GetUtcNow() - ProposalJobs.RejectionMemory,
                                    Pattern = id,
                                    p.Workspace,
                                },
                                cancellationToken: ct
                            )
                        )
                    ).Select(r => (object)new { Edits = JsonDocument.Parse(r.Edits).RootElement, Reason = Masking.Mask(r.Reason) }).ToList();
                    var row = await connection.QuerySingleAsync<(string Title, string Summary)>(
                        new CommandDefinition(
                            "SELECT title, summary FROM casebox.patterns WHERE org_id = @Org AND id = @Id",
                            new { Org = org, Id = id },
                            cancellationToken: ct
                        )
                    );
                    return Results.Ok(
                        new Evidence(
                            new
                            {
                                id,
                                row.Title,
                                row.Summary,
                                p.Key!.WentWrong,
                                p.Key.Label,
                                p.Key.Prevention,
                                p.Key.Path,
                            },
                            found,
                            cases,
                            rejections
                        )
                    );
                }
            )
            .RequireAuthorization(Policies.WorkerOrCi);
    }
}
