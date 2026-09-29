using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Ci;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

// The proposer's routes (docs/specs/self-evolution.md, Schedule and budget, and UI).
public static class ProposalEndpoints
{
    // The harness diet runs again when the last one is older than this.
    public static readonly TimeSpan DietEvery = TimeSpan.FromDays(90);

    public sealed record Started(string Pattern, string Proposal);

    public sealed record Skipped(string? Pattern, string Reason);

    // CiRun is the proposer run: casebox propose follows it, and its inline worker runs its jobs.
    public sealed record RunAnswer(
        string? CiRun,
        IReadOnlyList<Started> Started,
        IReadOnlyList<Skipped> Skipped,
        bool Diet
    );

    public sealed record RejectBody(string? Reason);

    public sealed record CandidateView(
        int Index,
        JsonElement Edits,
        IReadOnlyList<string> Files,
        string Rationale,
        IReadOnlyList<int>? MergedFrom,
        JsonElement? Score
    );

    public sealed record ProposalView(
        string Id,
        string Workspace,
        string? Pattern,
        string? PatternTitle,
        string Kind,
        string Status,
        string Repo,
        string BaseCommit,
        int? GateIndex,
        string? GateEvaluation,
        JsonElement? Checks,
        string? Reason,
        int? PrNumber,
        string? PrUrl,
        DateTimeOffset? MergedAt,
        JsonElement? Outcome,
        DateTimeOffset CreatedAt,
        DateTimeOffset UpdatedAt
    );

    public sealed record ProposalDetail(
        ProposalView Proposal,
        IReadOnlyList<CandidateView> Candidates
    );

    private sealed record Row(
        string Id,
        string Workspace,
        string? Pattern,
        string? PatternTitle,
        string Kind,
        string Status,
        string Repo,
        string BaseCommit,
        int? GateIndex,
        string? GateEvaluation,
        string? Checks,
        string? Reason,
        int? PrNumber,
        string? PrUrl,
        DateTime? MergedAt,
        string? Outcome,
        DateTime CreatedAt,
        DateTime UpdatedAt
    )
    {
        public ProposalView View() =>
            new(
                Id,
                Workspace,
                Pattern,
                PatternTitle,
                Kind,
                Status,
                Repo,
                BaseCommit,
                GateIndex,
                GateEvaluation,
                Json(Checks),
                Reason,
                PrNumber,
                PrUrl,
                MergedAt is { } m ? Utc(m) : null,
                Json(Outcome),
                Utc(CreatedAt),
                Utc(UpdatedAt)
            );
    }

    private const string Columns = """
        p.id, p.workspace, p.pattern, t.title AS pattern_title, p.kind, p.status, p.repo, p.base_commit, p.gate_index, p.gate_evaluation,
        p.checks::text AS checks, p.reason, p.pr_number, p.pr_url, p.merged_at, p.outcome::text AS outcome, p.created_at, p.updated_at
        FROM casebox.proposals p LEFT JOIN casebox.patterns t ON t.org_id = p.org_id AND t.id = p.pattern
        """;

    public static void MapProposals(this RouteGroupBuilder api)
    {
        api.MapPost(
                "/proposer/runs",
                async (ProposerRequest body, HttpContext http, ProposerRuns runs) =>
                    Results.Ok(await runs.StartAsync(body, http.User.Actor()!, http.RequestAborted))
            )
            .WithTags("Proposals")
            .RequireAuthorization(Policies.CiOrMember);

        var proposals = api.MapGroup("/proposals")
            .WithTags("Proposals")
            .RequireAuthorization(Policies.Viewer);

        proposals.MapGet(
            "/",
            async (string? workspace, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var rows = await connection.QueryAsync<Row>(
                    new CommandDefinition(
                        $"SELECT {Columns} WHERE p.org_id = @Org AND (@Workspace::text IS NULL OR p.workspace = @Workspace) ORDER BY p.created_at DESC LIMIT 200",
                        new { Org = http.User.OrgId(), Workspace = workspace },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(rows.Select(r => r.View()).ToList());
            }
        );

        proposals.MapGet(
            "/{id}",
            async (string id, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var row = await connection.QuerySingleOrDefaultAsync<Row>(
                    new CommandDefinition(
                        $"SELECT {Columns} WHERE p.org_id = @Org AND p.id = @Id",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                if (row is null)
                    return Results.NotFound();
                var candidates = await connection.QueryAsync<(
                    int Idx,
                    string Edits,
                    string Files,
                    string Rationale,
                    string? MergedFrom,
                    string? Score
                )>(
                    new CommandDefinition(
                        "SELECT idx, edits::text, files::text, rationale, merged_from::text, score::text FROM casebox.proposal_candidates WHERE org_id = @Org AND proposal_id = @Id ORDER BY idx",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(
                    new ProposalDetail(
                        row.View(),
                        candidates
                            .Select(c => new CandidateView(
                                c.Idx,
                                Json(c.Edits)!.Value,
                                JsonSerializer.Deserialize<string[]>(c.Files)!,
                                c.Rationale,
                                c.MergedFrom is null
                                    ? null
                                    : JsonSerializer.Deserialize<int[]>(c.MergedFrom),
                                Json(c.Score)
                            ))
                            .ToList()
                    )
                );
            }
        );

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
    }

    private static JsonElement? Json(string? text) =>
        text is null ? null : JsonDocument.Parse(text).RootElement.Clone();

    private static DateTimeOffset Utc(DateTime at) =>
        new(DateTime.SpecifyKind(at, DateTimeKind.Utc));
}

// Starts the proposer: a search for each open pattern of the workspace (or the one named), and the
// harness diet once a quarter, within the proposer's share of the monthly budget.
public sealed class ProposerRuns(
    NpgsqlDataSource db,
    IEventStore store,
    JobQueue jobs,
    DeedboxContext context,
    TimeProvider clock
)
{
    private string Org => context.TenantId;

    public async Task<ProposalEndpoints.RunAnswer> StartAsync(
        ProposerRequest body,
        string actor,
        CancellationToken ct
    )
    {
        if (body.Spec is null || string.IsNullOrWhiteSpace(body.Workspace))
            throw new DomainException("A proposer run names its workspace and baseline spec.");
        var spec = CiEndpoints.Normalize(body.Spec) with { Harness = "HEAD", Overrides = null };
        Planner.Check(spec, "baseline");
        if (body.BudgetRuns is < 2 or > 400)
            throw new DomainException("A search budget is 2 to 400 runs.");
        var (workspace, _) = await store.Load<Workspace>(Workspace.StreamIdFor(body.Workspace), ct);
        if (!workspace.Exists)
            throw new NotFoundException($"Workspace {body.Workspace} does not exist.");
        var repo = body.Repo is { } r
            ? WorkspaceDecider.NormalizeRepo(r)
            : workspace.Repos.FirstOrDefault();
        if (repo is null || !workspace.Repos.Contains(repo))
            throw new DomainException(
                $"The harness repository must be a repository of workspace {body.Workspace}."
            );
        var request = body with { Spec = spec, Repo = repo };

        await using var connection = await db.OpenConnectionAsync(ct);
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var now = clock.GetUtcNow();
        var share = org.Settings.Budgets.MonthlyUsd * org.Settings.ProposerBudgetShare;
        var spent = await ProposalSteps.ProposerSpendAsync(
            connection,
            Org,
            new DateTimeOffset(now.Year, now.Month, 1, 0, 0, 0, TimeSpan.Zero),
            ct
        );
        if (spent >= share)
            return new(
                null,
                [],
                [
                    new(
                        body.Pattern,
                        $"The proposer spent its share of this month's budget ({spent:0.00} of {share:0.00} USD)."
                    ),
                ],
                false
            );

        var patterns = (
            await connection.QueryAsync<(string Id, string Status, bool Advisory)>(
                new CommandDefinition(
                    """
                    SELECT id, status, advisory FROM casebox.patterns
                    WHERE org_id = @Org AND workspace = @Workspace AND (@Pattern::text IS NULL OR id = @Pattern)
                    ORDER BY detected_at, id
                    """,
                    new
                    {
                        Org,
                        body.Workspace,
                        body.Pattern,
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();
        if (body.Pattern is not null && patterns.Count == 0)
            throw new NotFoundException(
                $"Pattern {body.Pattern} is not a pattern of workspace {body.Workspace}."
            );

        // The proposer run: its jobs and evaluations carry its ID, so an inline worker can run them.
        var ciRun = Ids.New();
        await Ci.CiRuns.InsertAsync(
            connection,
            null,
            Org,
            new Ci.CiRun(
                ciRun,
                Ci.CiRuns.Proposer,
                body.Workspace,
                repo,
                null,
                null,
                null,
                null,
                null,
                Ci.CiRuns.Started,
                null,
                JsonSerializer.Serialize(request, EvaluationResults.Json),
                actor,
                now.UtcDateTime
            ),
            ct
        );
        var started = new List<ProposalEndpoints.Started>();
        var skipped = new List<ProposalEndpoints.Skipped>();
        var day = now.ToString("yyyy-MM-dd", System.Globalization.CultureInfo.InvariantCulture);
        foreach (var p in patterns)
        {
            if (p.Advisory || p.Status is "dismissed" or "resolved")
            {
                skipped.Add(
                    new(
                        p.Id,
                        p.Advisory
                            ? "advisory: a clearer ticket, a stronger model or tool access is not changed by pull request"
                            : $"the pattern is {p.Status}"
                    )
                );
                continue;
            }
            var busy = await connection.ExecuteScalarAsync<bool>(
                new CommandDefinition(
                    "SELECT EXISTS (SELECT 1 FROM casebox.proposals WHERE org_id = @Org AND pattern = @Pattern AND status IN ('searching', 'gating', 'gate_passed', 'pr_opened'))",
                    new { Org, Pattern = p.Id },
                    cancellationToken: ct
                )
            );
            if (busy)
            {
                skipped.Add(
                    new(p.Id, "a proposal for it is in flight or has an open pull request")
                );
                continue;
            }
            var proposal = Ids.New();
            if (
                await EnqueueAsync(
                    connection,
                    ProposalJobs.Search,
                    $"{ProposalJobs.Search}:{p.Id}:{day}",
                    p.Id,
                    [proposal],
                    request,
                    ciRun,
                    ct
                )
            )
                started.Add(new(p.Id, proposal));
            else
                skipped.Add(new(p.Id, "a search for it already started today"));
        }

        var diet = false;
        if (body.Pattern is null)
        {
            var last = await connection.ExecuteScalarAsync<DateTime?>(
                new CommandDefinition(
                    "SELECT max(created_at) FROM casebox.jobs WHERE org_id = @Org AND kind = @Kind AND payload->'request'->>'workspace' = @Workspace",
                    new
                    {
                        Org,
                        Kind = ProposalJobs.Diet,
                        body.Workspace,
                    },
                    cancellationToken: ct
                )
            );
            if (
                last is null
                || now - new DateTimeOffset(DateTime.SpecifyKind(last.Value, DateTimeKind.Utc))
                    > ProposalEndpoints.DietEvery
            )
            {
                var ids = Enumerable.Range(0, 20).Select(_ => Ids.New()).ToList();
                diet = await EnqueueAsync(
                    connection,
                    ProposalJobs.Diet,
                    $"{ProposalJobs.Diet}:{body.Workspace}:{day}",
                    null,
                    ids,
                    request,
                    ciRun,
                    ct
                );
            }
        }
        return new(ciRun, started, skipped, diet);
    }

    private async Task<bool> EnqueueAsync(
        System.Data.Common.DbConnection connection,
        string kind,
        string key,
        string? pattern,
        IReadOnlyList<string> proposals,
        ProposerRequest request,
        string ciRun,
        CancellationToken ct
    )
    {
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var exists = await connection.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                "SELECT EXISTS (SELECT 1 FROM casebox.jobs WHERE org_id = @Org AND idempotency_key = @Key)",
                new { Org, Key = key },
                transaction,
                cancellationToken: ct
            )
        );
        if (exists)
            return false;
        await jobs.EnqueueAsync(
            transaction,
            Org,
            kind,
            key,
            JsonSerializer.SerializeToElement(
                new
                {
                    pattern,
                    proposals,
                    repo = request.Repo,
                    request,
                    ciRun,
                },
                EvaluationResults.Json
            ),
            3,
            ct
        );
        await transaction.CommitAsync(ct);
        return true;
    }
}
