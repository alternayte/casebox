using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

// The proposal stream is the proposer's workflow (docs/specs/self-evolution.md, Search and The
// gate): a subscription scores every candidate on the dev batch, merges two winners that change
// different files, sends the best to the held-out gate, and records the gate's checks. Delivery is
// at least once; evaluation IDs come from the proposal, so every step is idempotent.
public sealed class ProposalWorkflow : Subscription
{
    public const string Name = "proposal_workflow";

    public ProposalWorkflow()
    {
        On<ProposalEvents.Drafted>((_, ctx) => Step(ctx, ctx.Envelope.StreamId));
        On<ProposalEvents.CandidateMerged>((_, ctx) => Step(ctx, ctx.Envelope.StreamId));
        On<ProposalEvents.CandidateScored>((_, ctx) => Step(ctx, ctx.Envelope.StreamId));
        On<EvaluationEvents.VerdictReached>(
            (e, ctx) =>
                e.Purpose is Purpose.Search or Purpose.Gate
                    ? ctx
                        .Services.GetRequiredService<ProposalSteps>()
                        .RecordAsync(ctx.Envelope.StreamId, ctx.CancellationToken)
                    : Task.CompletedTask
        );
    }

    private static Task Step(SubscriptionContext ctx, string streamId) =>
        ctx
            .Services.GetRequiredService<ProposalSteps>()
            .AdvanceAsync(streamId["proposal:".Length..], ctx.CancellationToken);
}

public sealed class ProposalSteps(
    IEventStore store,
    NpgsqlDataSource db,
    Planner planner,
    BlobStore blobs,
    DeedboxContext context,
    TimeProvider clock
)
{
    // A process check may be worse by at most this share before the gate fails it.
    public const double ProcessTolerance = 0.05;

    private string Org => context.TenantId;

    public static string SearchId(string proposal, int index) => $"{proposal}-c{index}";

    public static string GateId(string proposal) => $"{proposal}-gate";

    public async Task AdvanceAsync(string id, CancellationToken ct)
    {
        var (p, _) = await store.Load<Proposal>(Proposal.StreamId(id), ct);
        if (!p.Exists || p.Status != ProposalStatus.Searching)
            return;
        var draft = p.Draft!;
        if (draft.Batch.Count == 0)
        {
            await Inconclusive(
                id,
                "No dev case of the workspace has a baseline score yet; run casebox ci --baseline, then propose again.",
                ct
            );
            return;
        }

        // Every candidate is scored once, on the same batch.
        foreach (var c in p.Candidates.Where(c => !p.Scores.ContainsKey(c.Index)))
            await StartSearchAsync(id, p, c, ct);
        if (p.Candidates.Any(c => !p.Scores.ContainsKey(c.Index)))
            return;

        if (await MergeAsync(id, p, ct))
            return;

        var best = Best(p);
        if (best is null)
        {
            await Inconclusive(
                id,
                draft.Kind == ProposalKind.Removal
                    ? "The removal was not equivalent and cheaper on the dev batch."
                    : "No candidate did better than the baseline on the dev batch.",
                ct
            );
            return;
        }
        await StartGateAsync(id, p, best.Value, ct);
    }

    // The winner: the highest Δ (then the lowest cost) among candidates with Δ above 0; for a
    // removal, Δ of at least 0 at a cost ratio below 1.
    public static int? Best(Proposal p) =>
        p
            .Scores.Values.Where(s =>
                p.Draft!.Kind == ProposalKind.Removal
                    ? s.Delta >= 0 && s.CostRatio is < 1
                    : s.Delta > 0
            )
            .OrderByDescending(s => s.Delta)
            .ThenBy(s => s.CostRatio ?? 1)
            .ThenBy(s => s.Index)
            .Select(s => (int?)s.Index)
            .FirstOrDefault();

    private async Task StartSearchAsync(string id, Proposal p, Candidate c, CancellationToken ct)
    {
        var draft = p.Draft!;
        var evaluationId = SearchId(id, c.Index);
        var (existing, _) = await store.Load<Evaluation>(Evaluation.StreamId(evaluationId), ct);
        if (existing.Exists)
            return;
        await using var connection = await db.OpenConnectionAsync(ct);
        var scores = (
            await BaselineCache.ScoresAsync(
                connection,
                Org,
                draft.Workspace,
                HarnessSpec.Key(draft.Spec),
                null,
                ct
            )
        )
            .Where(s => draft.Batch.Contains(s.Key))
            .ToDictionary(s => s.Key, s => s.Value);
        try
        {
            if (scores.Count == 0)
                throw new DomainException("The batch's cases have no baseline score any more.");
            var plan = await planner.PlanAsync(
                new EvaluationRequest(
                    draft.Workspace,
                    draft.Spec,
                    draft.Spec with
                    {
                        Overrides = c.Overrides,
                    },
                    "dev",
                    [.. scores.Keys],
                    null,
                    1,
                    null,
                    null,
                    Purpose.Search,
                    draft.Prices
                ),
                draft.CiRun,
                scores,
                ct,
                id,
                c.Index
            );
            await RequireShareAsync(plan, ct);
            await store.Execute<Evaluation>(
                Evaluation.StreamId(evaluationId),
                e => EvaluationDecider.Request(e, plan.Requested),
                ct
            );
        }
        catch (DomainException ex) when (ex is not NotFoundException)
        {
            await Inconclusive(id, $"Candidate {c.Index} could not be scored: {ex.Message}", ct);
        }
    }

    // Two kept candidates that won on different cases and change different files are merged once,
    // when the search budget has room for one more score.
    private async Task<bool> MergeAsync(string id, Proposal p, CancellationToken ct)
    {
        var draft = p.Draft!;
        if (draft.Kind != ProposalKind.Edit || p.Candidates.Any(c => c.MergedFrom is not null))
            return false;
        var runs = p.Scores.Values.Sum(s => s.Runs);
        if (runs + draft.Batch.Count > draft.BudgetRuns)
            return false;
        var kept = p
            .Scores.Values.Where(s => s.Delta > 0 && s.Wins.Count > 0)
            .OrderByDescending(s => s.Delta)
            .ToList();
        foreach (var a in kept)
        foreach (var b in kept.Where(b => b.Index > a.Index))
        {
            if (a.Wins.ToHashSet().SetEquals(b.Wins))
                continue;
            var ca = p.Candidates.Single(c => c.Index == a.Index);
            var cb = p.Candidates.Single(c => c.Index == b.Index);
            if (ca.Edits.Count + cb.Edits.Count > ProposalDecider.MaxEdits)
                continue;
            var fa = await FilesAsync(ca.Overrides, ct);
            var fb = await FilesAsync(cb.Overrides, ct);
            if (fa.Files.Keys.Intersect(fb.Files.Keys).Any())
                continue;
            var merged = new Overrides(fa.Repo, fa.Files.Concat(fb.Files).ToDictionary());
            var hash = await PutOverridesAsync(merged, ct);
            var edits = ca.Edits.Concat(cb.Edits).ToList();
            var candidate = new Candidate(
                p.Candidates.Max(c => c.Index) + 1,
                edits,
                hash,
                ContentHash(edits),
                $"{ca.Rationale} {cb.Rationale}",
                [a.Index, b.Index]
            );
            await store.Execute<Proposal>(
                Proposal.StreamId(id),
                s => ProposalDecider.Merge(s, candidate),
                ct
            );
            return true;
        }
        return false;
    }

    private async Task StartGateAsync(string id, Proposal p, int index, CancellationToken ct)
    {
        var draft = p.Draft!;
        var candidate = p.Candidates.Single(c => c.Index == index);
        var evaluationId = GateId(id);
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var budget = Suite.DefaultBudget;
        try
        {
            await RotateIfSpentAsync(draft.Workspace, id, budget, ct);
            var plan = await planner.PlanAsync(
                new EvaluationRequest(
                    draft.Workspace,
                    draft.Spec,
                    draft.Spec with
                    {
                        Overrides = candidate.Overrides,
                    },
                    "held_out",
                    null,
                    null,
                    draft.Repeats,
                    null,
                    null,
                    Purpose.Gate,
                    draft.Prices
                ),
                draft.CiRun,
                null,
                ct,
                id,
                index
            );
            Planner.RequireMonthlyRoom(plan);
            await RequireShareAsync(plan, ct);

            await using var connection = await db.OpenConnectionAsync(ct);
            await using var transaction = await connection.BeginTransactionAsync(ct);
            var tx = store.UseTransaction(transaction);
            await tx.Execute<Suite>(
                Suite.StreamId(draft.Workspace),
                s => SuiteDecider.Query(s, id, evaluationId, budget),
                ct
            );
            await tx.Execute<Evaluation>(
                Evaluation.StreamId(evaluationId),
                e => EvaluationDecider.Request(e, plan.Requested),
                ct
            );
            await tx.Execute<Proposal>(
                Proposal.StreamId(id),
                s => ProposalDecider.RequestGate(s, index, evaluationId),
                ct
            );
            await transaction.CommitAsync(ct);
        }
        catch (DomainException ex) when (ex is not NotFoundException)
        {
            await Inconclusive(id, $"The gate could not run: {ex.Message}", ct);
        }
    }

    // A spent held-out budget rotates the suite: every held-out case goes to dev, and as many
    // approved dev cases as there were go to held-out, newest first, never one the search of this
    // rotation ran on.
    internal async Task RotateIfSpentAsync(
        string workspace,
        string proposal,
        int budget,
        CancellationToken ct
    )
    {
        var (suite, _) = await store.Load<Suite>(Suite.StreamId(workspace), ct);
        if (!suite.Spent(budget))
            return;
        await using var connection = await db.OpenConnectionAsync(ct);
        var heldOut = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    "SELECT id FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace AND status = 'approved' AND split = 'held_out' ORDER BY id",
                    new { Org, Workspace = workspace },
                    cancellationToken: ct
                )
            )
        ).ToList();
        var searched = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    """
                    SELECT DISTINCT r.case_id FROM casebox.run_results r JOIN casebox.evaluations e ON e.org_id = r.org_id AND e.id = r.evaluation_id
                    WHERE r.org_id = @Org AND e.workspace = @Workspace AND e.purpose = 'search' AND e.created_at >= @Since
                    """,
                    new
                    {
                        Org,
                        Workspace = workspace,
                        Since = suite.Since,
                    },
                    cancellationToken: ct
                )
            )
        ).ToHashSet();
        var toHeldOut = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    "SELECT id FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace AND status = 'approved' AND split = 'dev' ORDER BY mined_at DESC, id",
                    new { Org, Workspace = workspace },
                    cancellationToken: ct
                )
            )
        )
            .Where(c => !searched.Contains(c))
            .Take(heldOut.Count)
            .ToList();

        await using var transaction = await connection.BeginTransactionAsync(ct);
        var tx = store.UseTransaction(transaction);
        await tx.Execute<Suite>(
            Suite.StreamId(workspace),
            s => SuiteDecider.Rotate(s, toHeldOut, heldOut, budget, clock.GetUtcNow()),
            ct
        );
        foreach (var c in heldOut)
            await tx.Execute<Case>(Case.StreamId(c), x => CaseDecider.Rotate(x, CaseSplit.Dev), ct);
        foreach (var c in toHeldOut)
            await tx.Execute<Case>(
                Case.StreamId(c),
                x => CaseDecider.Rotate(x, CaseSplit.HeldOut),
                ct
            );
        await transaction.CommitAsync(ct);
    }

    // The proposer's spend this month, with the new estimate, stays within its share of the
    // organisation's monthly budget.
    private async Task RequireShareAsync(Plan plan, CancellationToken ct)
    {
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var share = org.Settings.Budgets.MonthlyUsd * org.Settings.ProposerBudgetShare;
        var now = clock.GetUtcNow();
        await using var connection = await db.OpenConnectionAsync(ct);
        var spent = await ProposerSpendAsync(
            connection,
            Org,
            new DateTimeOffset(now.Year, now.Month, 1, 0, 0, 0, TimeSpan.Zero),
            ct
        );
        if (spent + plan.Requested.Estimate.TotalUsd > share)
            throw new DomainException(
                $"The proposer's spend this month ({spent:0.00} USD) plus this estimate ({plan.Requested.Estimate.TotalUsd:0.00} USD) passes its share of the monthly budget ({share:0.00} USD)."
            );
    }

    public static Task<decimal> ProposerSpendAsync(
        System.Data.Common.DbConnection connection,
        string org,
        DateTimeOffset since,
        CancellationToken ct
    ) =>
        connection.ExecuteScalarAsync<decimal>(
            new CommandDefinition(
                """
                SELECT coalesce(sum(r.cost_usd), 0) FROM casebox.run_results r
                JOIN casebox.evaluations e ON e.org_id = r.org_id AND e.id = r.evaluation_id
                WHERE r.org_id = @Org AND e.purpose IN ('search', 'gate') AND r.created_at >= @Since
                """,
                new { Org = org, Since = since },
                cancellationToken: ct
            )
        );

    // A search or gate evaluation reached its verdict: its score or the gate's checks go on the
    // proposal.
    public async Task RecordAsync(string evaluationStream, CancellationToken ct)
    {
        var (e, _) = await store.Load<Evaluation>(evaluationStream, ct);
        var request = e.Request;
        if (request?.Proposal is not { } id || request.CandidateIndex is not { } index)
            return;
        await using var connection = await db.OpenConnectionAsync(ct);
        var verdict = await VerdictAsync(
            connection,
            Org,
            evaluationStream["evaluation:".Length..],
            ct
        );
        if (verdict is null)
            return;

        if (request.Purpose == Purpose.Search)
        {
            var scores = request.BaselineScores ?? new Dictionary<string, BaselineScore>();
            var wins = e
                .Cases.Where(c =>
                {
                    var runs = e
                        .Runs.Values.Where(r =>
                            r.CaseId == c.CaseId && r.Side == Side.Candidate && !r.Failed
                        )
                        .ToList();
                    if (
                        runs.Count == 0
                        || !scores.TryGetValue(c.CaseId, out var b)
                        || b.Passed.Count == 0
                    )
                        return false;
                    return (double)runs.Count(r => r.Passed == true) / runs.Count
                        > (double)b.Passed.Count(x => x) / b.Passed.Count;
                })
                .Select(c => c.CaseId)
                .ToList();
            await store.Execute<Proposal>(
                Proposal.StreamId(id),
                s =>
                    ProposalDecider.Score(
                        s,
                        new ProposalEvents.CandidateScored(
                            index,
                            evaluationStream["evaluation:".Length..],
                            verdict.Delta,
                            verdict.Lower,
                            verdict.Upper,
                            wins,
                            verdict.CostRatio,
                            e.Runs.Values.Count(r => !r.Failed)
                        )
                    ),
                ct
            );
            return;
        }

        var (p, _) = await store.Load<Proposal>(Proposal.StreamId(id), ct);
        var checks = await ChecksAsync(connection, p, e, verdict, ct);
        var evaluationId = evaluationStream["evaluation:".Length..];
        object outcome =
            verdict.Verdict == Statistics.Verdict.Inconclusive
                ? new ProposalEvents.GateInconclusive(
                    evaluationId,
                    checks,
                    "The gate's verdict is inconclusive, so no pull request opens; the candidate is parked with its evidence."
                )
            : checks.All(c => c.Passed) ? new ProposalEvents.GatePassed(evaluationId, checks)
            : new ProposalEvents.GateFailed(evaluationId, checks);
        await store.Execute<Proposal>(
            Proposal.StreamId(id),
            s => ProposalDecider.Conclude(s, outcome),
            ct
        );
    }

    // The gate's three checks (SDD section 9, The gate), each with its numbers.
    private async Task<IReadOnlyList<GateCheck>> ChecksAsync(
        System.Data.Common.DbConnection connection,
        Proposal p,
        Evaluation e,
        EvaluationEvents.VerdictReached v,
        CancellationToken ct
    )
    {
        var inv = System.Globalization.CultureInfo.InvariantCulture;
        var checks = new List<GateCheck>
        {
            new(
                "quality",
                v.Verdict is Statistics.Verdict.Better or Statistics.Verdict.Equivalent,
                "strong",
                string.Create(
                    inv,
                    $"Verdict {v.Verdict.ToString().ToLowerInvariant()}: Δ {v.Delta * 100:+0.0;-0.0} points, {v.Level * 100:0.#}% interval {v.Lower * 100:+0.0;-0.0} to {v.Upper * 100:+0.0;-0.0}, {v.Cases} cases."
                )
            ),
        };

        if (p.Draft!.Kind == ProposalKind.Removal)
            checks.Add(
                new(
                    "cheaper",
                    v.EquivalentAndCheaper || v.Verdict == Statistics.Verdict.Better,
                    "strong",
                    v.CostRatio is { } r
                        ? string.Create(
                            inv,
                            $"Cost ratio {r:0.00} (interval {v.CostLower:0.00} to {v.CostUpper:0.00})."
                        )
                        : "No cost data."
                )
            );
        else
        {
            var refs = (
                await store.Load<Patterns.Pattern>(Patterns.Pattern.StreamId(p.Draft.Pattern!), ct)
            )
                .State
                .Refs;
            var own = (
                await connection.QueryAsync<string>(
                    new CommandDefinition(
                        "SELECT id FROM casebox.case_catalog WHERE org_id = @Org AND id = ANY(@Ids) AND source = ANY(@Sources)",
                        new
                        {
                            Org,
                            Ids = e.Cases.Select(c => c.CaseId).ToArray(),
                            Sources = refs.Select(r => $"steering:{r}").ToArray(),
                        },
                        cancellationToken: ct
                    )
                )
            ).ToHashSet();
            double Rate(Side side)
            {
                var runs = e
                    .Runs.Values.Where(r => own.Contains(r.CaseId) && r.Side == side && !r.Failed)
                    .ToList();
                return runs.Count == 0 ? 0 : (double)runs.Count(r => r.Passed == true) / runs.Count;
            }
            var improved = own.Count > 0 && Rate(Side.Candidate) > Rate(Side.Baseline);
            checks.Add(
                new(
                    "pattern",
                    improved || v.EquivalentAndCheaper,
                    "strong",
                    own.Count == 0
                        ? v.EquivalentAndCheaper
                            ? "The pattern has no held-out case; the candidate is equivalent and cheaper."
                            : "The pattern has no held-out case, and the candidate is not equivalent and cheaper."
                        : string.Create(
                            inv,
                            $"The pattern's {own.Count} held-out cases: candidate {Rate(Side.Candidate) * 100:0}% passed, baseline {Rate(Side.Baseline) * 100:0}%."
                        )
                )
            );
        }

        var process = (
            await connection.QueryAsync<(string Side, bool? RanTests, bool? EditedTest)>(
                new CommandDefinition(
                    """
                    SELECT side, (process_checks->>'ranTestsBeforeDone')::boolean, (process_checks->>'editedTestAfterFailure')::boolean
                    FROM casebox.run_results WHERE org_id = @Org AND evaluation_id = @Id AND status = 'completed' AND process_checks IS NOT NULL
                    """,
                    new { Org, Id = GateId(e.Request!.Proposal!) },
                    cancellationToken: ct
                )
            )
        ).ToList();
        double Share(string side, Func<(string Side, bool? RanTests, bool? EditedTest), bool> f)
        {
            var rows = process.Where(r => r.Side == side).ToList();
            return rows.Count == 0 ? 0 : (double)rows.Count(f) / rows.Count;
        }
        var ranB = Share("baseline", r => r.RanTests == true);
        var ranC = Share("candidate", r => r.RanTests == true);
        var editB = Share("baseline", r => r.EditedTest == true);
        var editC = Share("candidate", r => r.EditedTest == true);
        checks.Add(
            new(
                "process",
                ranC >= ranB - ProcessTolerance && editC <= editB + ProcessTolerance,
                "strong",
                string.Create(
                    inv,
                    $"Ran the tests before saying done: candidate {ranC * 100:0}%, baseline {ranB * 100:0}%. Edited a test after a failure: candidate {editC * 100:0}%, baseline {editB * 100:0}%."
                )
            )
        );
        return checks;
    }

    private static async Task<EvaluationEvents.VerdictReached?> VerdictAsync(
        System.Data.Common.DbConnection connection,
        string org,
        string evaluationId,
        CancellationToken ct
    )
    {
        var json = await connection.ExecuteScalarAsync<string?>(
            new CommandDefinition(
                "SELECT verdict::text FROM casebox.evaluations WHERE org_id = @Org AND id = @Id AND verdict IS NOT NULL",
                new { Org = org, Id = evaluationId },
                cancellationToken: ct
            )
        );
        return json is null
            ? null
            : JsonSerializer.Deserialize<EvaluationEvents.VerdictReached>(
                json,
                EvaluationResults.Json
            );
    }

    private Task Inconclusive(string id, string reason, CancellationToken ct) =>
        store.Execute<Proposal>(
            Proposal.StreamId(id),
            s => ProposalDecider.Conclude(s, new ProposalEvents.GateInconclusive(null, [], reason)),
            ct
        );

    public sealed record Overrides(string Repo, Dictionary<string, string?> Files);

    public async Task<Overrides> FilesAsync(string hash, CancellationToken ct)
    {
        var blob =
            await blobs.GetAsync(Org, hash, ct)
            ?? throw new DomainException($"The overrides {hash[..12]} are gone.");
        return JsonSerializer.Deserialize<Overrides>(blob.Data, EvaluationResults.Json)!;
    }

    public async Task<string> PutOverridesAsync(Overrides overrides, CancellationToken ct)
    {
        var data = JsonSerializer.SerializeToUtf8Bytes(overrides, EvaluationResults.Json);
        var hash = BlobStore.HashOf(data);
        await blobs.PutAsync(Org, hash, "application/json", data, ct);
        return hash;
    }

    // SHA-256 of a candidate's edits in a stable order: the same change has the same hash.
    public static string ContentHash(IEnumerable<Edit> edits) =>
        Convert.ToHexStringLower(
            SHA256.HashData(
                Encoding.UTF8.GetBytes(
                    string.Join(
                        "\n",
                        edits
                            .Select(x => JsonSerializer.Serialize(x, EvaluationResults.Json))
                            .Order(StringComparer.Ordinal)
                    )
                )
            )
        );
}
