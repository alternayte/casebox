using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Evaluations;

// What a person asks for: two sides that differ in one thing, on a workspace's approved cases.
public sealed record EvaluationRequest(
    string Workspace,
    HarnessSpec Baseline,
    HarnessSpec Candidate,
    string? Split,
    IReadOnlyList<string>? CaseIds,
    int? MaxCases,
    int? Repeats,
    double? Delta,
    decimal? CapUsd,
    Purpose? Purpose,
    IReadOnlyDictionary<string, Price>? Prices
);

public sealed record Plan(
    EvaluationEvents.Requested Requested,
    decimal MonthSpentUsd,
    decimal MonthlyUsd,
    decimal ConfirmAboveUsd
);

// Turns a request into the requested event: the cases, the estimate and the budget checks of SDD
// section 8. Nothing runs from here; the estimate is shown before any run.
public sealed class Planner(
    NpgsqlDataSource db,
    IEventStore store,
    DeedboxContext context,
    TimeProvider clock
)
{
    // Without history, a run is assumed to use this many tokens and minutes.
    public const long DefaultTokens = 400_000;
    public const double DefaultMinutes = 12;

    // How a run's tokens split when the case has no history: most input is read from the cache.
    private const double DefaultInputShare = 0.35;
    private const double DefaultCacheReadShare = 0.60;
    private const double DefaultOutputShare = 0.05;

    private string Org => context.TenantId;

    // A person's request. Baseline and harness CI evaluations come only from casebox ci.
    public Task<Plan> PlanAsync(EvaluationRequest body, CancellationToken ct)
    {
        if (body.Purpose is Purpose.Baseline or Purpose.HarnessCi or Purpose.Search or Purpose.Gate)
            throw new DomainException(
                "Baseline and harness CI evaluations are started by casebox ci, and search and gate evaluations by the proposer, not requested directly."
            );
        return PlanAsync(body, null, null, ct);
    }

    // ciRun and scores are set by harness CI (docs/specs/harness-ci.md).
    public async Task<Plan> PlanAsync(
        EvaluationRequest body,
        string? ciRun,
        IReadOnlyDictionary<string, BaselineScore>? scores,
        CancellationToken ct,
        string? proposal = null,
        int? candidate = null
    )
    {
        if (string.IsNullOrWhiteSpace(body.Workspace))
            throw new DomainException("Name the workspace to evaluate on.");
        if (body.Baseline is null || body.Candidate is null)
            throw new DomainException("An evaluation needs a baseline and a candidate.");
        // Settings may be left out of a request; every setting then takes its default.
        var request = body with
        {
            Baseline = body.Baseline with
            {
                Settings = body.Baseline.Settings ?? new AgentSettings(null, null, null),
            },
            Candidate = body.Candidate with
            {
                Settings = body.Candidate.Settings ?? new AgentSettings(null, null, null),
            },
        };
        Check(request.Baseline, "baseline");
        Check(request.Candidate, "candidate");
        var purpose = request.Purpose ?? Purpose.Compare;
        var changes = HarnessSpec.Changes(request.Baseline, request.Candidate);
        if (purpose == Purpose.Baseline)
        {
            if (changes.Count != 0)
                throw new DomainException(
                    "A baseline evaluation has one side: its candidate is its baseline."
                );
            changes = ["none"];
        }
        else if (purpose is Purpose.HarnessCi or Purpose.Search && changes is not ["harness"])
            throw new DomainException(
                "Harness CI and proposal search change the harness and nothing else."
            );
        else if (changes.Count != 1)
            throw new DomainException(
                changes.Count == 0
                    ? "The two sides are the same; an evaluation changes exactly one thing."
                    : $"The two sides differ in {string.Join(", ", changes)}; an evaluation changes exactly one thing."
            );

        var split = request.Split ?? "dev";
        if (split is not ("dev" or "held_out"))
            throw new DomainException("The split is dev or held_out.");
        if (split == "held_out" && purpose != Purpose.Gate)
            throw new DomainException("Only the proposal gate evaluates on the held-out split.");
        if (purpose == Purpose.HarnessVsNone && request.Candidate.Harness != "none")
            throw new DomainException("Harness versus none has 'none' as the candidate's harness.");

        var prices = request.Prices ?? new Dictionary<string, Price>();
        foreach (var model in new[] { request.Baseline.Model, request.Candidate.Model }.Distinct())
            if (!prices.ContainsKey(model))
                throw new DomainException(
                    $"casebox.yml has no price for {model}. Add it under prices (USD per million tokens); there is no live price lookup.",
                    Cbx.MissingPrice
                );

        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var budgets = org.Settings.Budgets;
        var cap = Math.Min(request.CapUsd ?? budgets.PerEvaluationUsd, budgets.PerEvaluationUsd);
        var repeats = request.Repeats ?? 3;

        await using var connection = await db.OpenConnectionAsync(ct);
        var cases = (
            await connection.QueryAsync<(string Id, double Weight, double? Seconds)>(
                new CommandDefinition(
                    """
                    SELECT id, weight, seconds FROM casebox.case_catalog
                    WHERE org_id = @Org AND workspace = @Workspace AND status = 'approved' AND split = @Split
                      AND instruction IS NOT NULL AND (@Ids::text[] IS NULL OR id = ANY(@Ids))
                    ORDER BY drift, mined_at DESC, id
                    """,
                    new
                    {
                        Org,
                        request.Workspace,
                        Split = split,
                        Ids = request.CaseIds is { Count: > 0 } ids ? ids.ToArray() : null,
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();
        if (request.MaxCases is > 0 and var max)
            cases = cases.Take(max).ToList();
        if (cases.Count == 0)
            throw new DomainException(
                $"Workspace {request.Workspace} has no approved {split} case to evaluate.",
                Cbx.NoCases
            );

        var history = (
            await connection.QueryAsync<(
                string CaseId,
                double Tokens,
                double Input,
                double CacheRead,
                double Output,
                double Minutes
            )>(
                new CommandDefinition(
                    """
                    SELECT case_id,
                           percentile_cont(0.5) WITHIN GROUP (ORDER BY coalesce((usage->>'inputTokens')::float8, 0) + coalesce((usage->>'outputTokens')::float8, 0)
                               + coalesce((usage->>'cacheReadTokens')::float8, 0) + coalesce((usage->>'cacheWriteTokens')::float8, 0)),
                           percentile_cont(0.5) WITHIN GROUP (ORDER BY coalesce((usage->>'inputTokens')::float8, 0) + coalesce((usage->>'cacheWriteTokens')::float8, 0)),
                           percentile_cont(0.5) WITHIN GROUP (ORDER BY coalesce((usage->>'cacheReadTokens')::float8, 0)),
                           percentile_cont(0.5) WITHIN GROUP (ORDER BY coalesce((usage->>'outputTokens')::float8, 0)),
                           percentile_cont(0.5) WITHIN GROUP (ORDER BY seconds) / 60
                    FROM casebox.run_results WHERE org_id = @Org AND case_id = ANY(@Ids) AND status = 'completed' AND usage IS NOT NULL
                    GROUP BY case_id
                    """,
                    new { Org, Ids = cases.Select(c => c.Id).ToArray() },
                    cancellationToken: ct
                )
            )
        ).ToDictionary(h => h.CaseId);

        var runsBaseline = purpose is not (Purpose.HarnessCi or Purpose.Search);
        var runsCandidate = purpose != Purpose.Baseline;
        var sides = (runsBaseline ? 1 : 0) + (runsCandidate ? 1 : 0);
        long baselineTokens = 0,
            candidateTokens = 0;
        decimal baselineUsd = 0,
            candidateUsd = 0;
        double sandboxMinutes = 0,
            longest = 0;
        foreach (var c in cases)
        {
            var (input, cacheRead, output, minutes) =
                history.TryGetValue(c.Id, out var h) && h.Tokens > 0
                    ? (h.Input, h.CacheRead, h.Output, h.Minutes)
                    : (
                        DefaultTokens * DefaultInputShare,
                        DefaultTokens * DefaultCacheReadShare,
                        DefaultTokens * DefaultOutputShare,
                        DefaultMinutes
                    );
            var verifyMinutes = (c.Seconds ?? 120) / 60 + 1;
            var tokens = (long)(input + cacheRead + output) * repeats;
            if (runsBaseline)
            {
                baselineTokens += tokens;
                baselineUsd +=
                    Cost(prices[request.Baseline.Model], input, cacheRead, output) * repeats;
            }
            if (runsCandidate)
            {
                candidateTokens += tokens;
                candidateUsd +=
                    Cost(prices[request.Candidate.Model], input, cacheRead, output) * repeats;
            }
            sandboxMinutes += (minutes + verifyMinutes) * sides * repeats;
            longest = Math.Max(longest, minutes + verifyMinutes);
        }

        var total = Math.Round(baselineUsd + candidateUsd, 2);
        var estimate = new Estimate(
            cases.Count,
            cases.Count * sides * repeats,
            baselineTokens,
            candidateTokens,
            Math.Round(baselineUsd, 2),
            Math.Round(candidateUsd, 2),
            total,
            Math.Round(sandboxMinutes, 1),
            Math.Round(longest * repeats, 1),
            Math.Round(sandboxMinutes, 1),
            Math.Round(total / repeats, 2),
            purpose != Purpose.Baseline && repeats is >= 1 and <= 10
                ? Statistics.DetectableEffect(cases.Count, repeats)
                : null
        );

        var monthStart = new DateTimeOffset(
            clock.GetUtcNow().Year,
            clock.GetUtcNow().Month,
            1,
            0,
            0,
            0,
            TimeSpan.Zero
        );
        var monthSpent = await connection.ExecuteScalarAsync<decimal>(
            new CommandDefinition(
                "SELECT coalesce(sum(cost_usd), 0) FROM casebox.run_results WHERE org_id = @Org AND created_at >= @Since",
                new { Org, Since = monthStart },
                cancellationToken: ct
            )
        );

        var requested = new EvaluationEvents.Requested(
            request.Workspace,
            split,
            cases.Select(c => new EvaluationCase(c.Id, c.Weight)).ToList(),
            request.Baseline,
            request.Candidate,
            changes[0],
            repeats,
            request.Delta ?? 0.05,
            cap,
            estimate,
            purpose,
            HarnessSpec.MutableModel(request.Baseline.Model)
                || HarnessSpec.MutableModel(request.Candidate.Model),
            total > budgets.ConfirmAboveUsd,
            prices
                .Where(p => p.Key == request.Baseline.Model || p.Key == request.Candidate.Model)
                .ToDictionary(p => p.Key, p => p.Value),
            ciRun,
            scores,
            proposal,
            candidate
        );
        return new Plan(requested, monthSpent, budgets.MonthlyUsd, budgets.ConfirmAboveUsd);
    }

    // The monthly limit holds across evaluations: this estimate plus the month's spend.
    public static void RequireMonthlyRoom(Plan plan)
    {
        if (plan.MonthSpentUsd + plan.Requested.Estimate.TotalUsd > plan.MonthlyUsd)
            throw new DomainException(
                $"This month's spend of {plan.MonthSpentUsd:0.00} USD plus the estimate of {plan.Requested.Estimate.TotalUsd:0.00} USD is over the monthly limit of {plan.MonthlyUsd:0.00} USD.",
                Cbx.MonthlyBudget
            );
    }

    public static decimal Cost(Price price, double input, double cacheRead, double output) =>
        (
            (decimal)input * price.Input
            + (decimal)cacheRead * (price.CacheRead ?? price.Input)
            + (decimal)output * price.Output
        ) / 1_000_000m;

    public static void Check(HarnessSpec spec, string side)
    {
        if (!HarnessSpec.Agents.Contains(spec.Agent))
            throw new DomainException(
                $"The {side} agent '{spec.Agent}' is not claude-code, codex, cursor-cli or command."
            );
        if (string.IsNullOrWhiteSpace(spec.AgentVersion) && spec.Agent != "command")
            throw new DomainException($"The {side} names no agent version; versions are pinned.");
        if (string.IsNullOrWhiteSpace(spec.Model))
            throw new DomainException($"The {side} names no model.");
        if (string.IsNullOrWhiteSpace(spec.Harness))
            throw new DomainException($"The {side} names no harness: a git ref, or none.");
        if (spec.Agent == "command" && string.IsNullOrWhiteSpace(spec.Command?.Template))
            throw new DomainException($"The {side} uses the command agent without a template.");
        if (spec.Shared is { } shared)
        {
            if (!HarnessSpec.SharedAgents.Contains(spec.Agent))
                throw new DomainException(
                    $"The {side} uses a shared harness, but {spec.Agent} has no user-level configuration to put it in; only claude-code and codex do.",
                    Cbx.SharedHarnessAgent
                );
            if (string.IsNullOrWhiteSpace(shared.Repo) || string.IsNullOrWhiteSpace(shared.Ref))
                throw new DomainException(
                    $"The {side}'s shared harness names no repository or ref."
                );
        }
        if (spec.Settings.TimeoutMinutes is < 1 or > 240)
            throw new DomainException("A run's timeout is 1 to 240 minutes.");
    }
}
