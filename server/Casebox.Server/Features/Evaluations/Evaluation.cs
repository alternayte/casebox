using System.Collections.Immutable;
using Deedbox;

namespace Casebox.Server.Features.Evaluations;

public enum Side
{
    Baseline,
    Candidate,
}

public enum Purpose
{
    Compare,
    HarnessVsNone,
    HarnessCi,
    Gate,
}

public enum EvaluationStatus
{
    AwaitingConfirmation,
    Running,
    Done,
    Cancelled,
}

public sealed record AgentSettings(int? MaxTurns, int? TimeoutMinutes, long? TokenCap);

public sealed record CommandTemplate(string Template, string? LogGlob, string? LogFormat);

// One side of an evaluation (SDD section 8, Harness specification).
public sealed record HarnessSpec(
    string Agent,
    string AgentVersion,
    string Model,
    string? Effort,
    string Harness,
    AgentSettings Settings,
    CommandTemplate? Command
)
{
    public static readonly string[] Agents = ["claude-code", "codex", "cursor-cli", "command"];

    // What differs between two sides. An evaluation changes exactly one thing.
    public static IReadOnlyList<string> Changes(HarnessSpec a, HarnessSpec b)
    {
        var changes = new List<string>();
        // Another agent comes with its own version: that is one change.
        if (a.Agent != b.Agent)
            changes.Add("agent");
        else if (a.AgentVersion != b.AgentVersion)
            changes.Add("agentVersion");
        if (a.Model != b.Model)
            changes.Add("model");
        if (a.Effort != b.Effort)
            changes.Add("effort");
        if (a.Harness != b.Harness)
            changes.Add("harness");
        if (a.Settings != b.Settings)
            changes.Add("settings");
        if (a.Command != b.Command)
            changes.Add("command");
        return changes;
    }

    // A model ID that names no fixed version: an alias, "latest", or no date or version number.
    public static bool MutableModel(string model)
    {
        var m = model.ToLowerInvariant();
        if (m.Contains("latest", StringComparison.Ordinal))
            return true;
        var name = m[(m.LastIndexOf('/') + 1)..];
        return !name.Any(char.IsDigit);
    }
}

// The estimate shown before any run (SDD section 8, Cost control).
public sealed record Estimate(
    int Cases,
    int Runs,
    long BaselineTokens,
    long CandidateTokens,
    decimal BaselineUsd,
    decimal CandidateUsd,
    decimal TotalUsd,
    double SandboxMinutes,
    double MinMinutes,
    double MaxMinutes,
    decimal PerRoundUsd
);

// A case in the evaluation, with the weight its validation gave it.
public sealed record EvaluationCase(string CaseId, double Weight);

public static class EvaluationEvents
{
    public sealed record Requested(
        string Workspace,
        string Split,
        IReadOnlyList<EvaluationCase> Cases,
        HarnessSpec Baseline,
        HarnessSpec Candidate,
        string Change,
        int Repeats,
        double Delta,
        decimal CapUsd,
        Estimate Estimate,
        Purpose Purpose,
        bool MutableModel,
        bool NeedsConfirmation,
        IReadOnlyDictionary<string, Price> Prices
    );

    public sealed record Confirmed(string By);

    public sealed record RunCompleted(
        string RunId,
        string CaseId,
        Side Side,
        int Repeat,
        bool Passed,
        decimal CostUsd,
        double Seconds,
        long Tokens,
        string Evidence
    );

    public sealed record RunFailed(
        string RunId,
        string CaseId,
        Side Side,
        int Repeat,
        string Reason
    );

    public sealed record CheckpointEvaluated(
        int Round,
        double Level,
        int Cases,
        double Delta,
        double Lower,
        double Upper,
        Statistics.Verdict Verdict
    );

    public sealed record VerdictReached(
        Statistics.Verdict Verdict,
        double Delta,
        double Lower,
        double Upper,
        double Level,
        int Cases,
        int Runs,
        double? CostRatio,
        double? CostLower,
        double? CostUpper,
        double? DurationRatio,
        double? DurationLower,
        double? DurationUpper,
        bool EquivalentAndCheaper,
        double BaselineRate,
        double CandidateRate,
        string? Reason
    );

    public sealed record BudgetExhausted(decimal SpentUsd);

    public sealed record Cancelled(string By, string Reason);
}

// USD per million tokens, from the prices table of casebox.yml.
public sealed record Price(decimal Input, decimal Output, decimal? CacheRead, decimal? CacheWrite);

public sealed record RunState(
    string CaseId,
    Side Side,
    int Repeat,
    bool? Passed,
    bool Failed,
    decimal CostUsd,
    double Seconds
);

public sealed record Evaluation(
    bool Exists,
    EvaluationStatus Status,
    int Repeats,
    double Delta,
    decimal CapUsd,
    decimal SpentUsd,
    ImmutableList<EvaluationCase> Cases,
    ImmutableDictionary<string, RunState> Runs,
    ImmutableHashSet<int> Checkpoints,
    bool Verdict,
    bool Exhausted,
    EvaluationEvents.Requested? Request
) : IState<Evaluation>
{
    public static Evaluation Initial { get; } =
        new(
            false,
            EvaluationStatus.AwaitingConfirmation,
            0,
            0,
            0,
            0,
            [],
            ImmutableDictionary<string, RunState>.Empty,
            [],
            false,
            false,
            null
        );

    public static Evaluation Evolve(Evaluation s, object e) =>
        e switch
        {
            EvaluationEvents.Requested x => s with
            {
                Exists = true,
                Status = x.NeedsConfirmation
                    ? EvaluationStatus.AwaitingConfirmation
                    : EvaluationStatus.Running,
                Repeats = x.Repeats,
                Delta = x.Delta,
                CapUsd = x.CapUsd,
                Cases = [.. x.Cases],
                Request = x,
            },
            EvaluationEvents.Confirmed => s with { Status = EvaluationStatus.Running },
            EvaluationEvents.RunCompleted x => s with
            {
                SpentUsd = s.SpentUsd + x.CostUsd,
                Runs = s.Runs.SetItem(
                    x.RunId,
                    new RunState(x.CaseId, x.Side, x.Repeat, x.Passed, false, x.CostUsd, x.Seconds)
                ),
            },
            EvaluationEvents.RunFailed x => s with
            {
                Runs = s.Runs.SetItem(
                    x.RunId,
                    new RunState(x.CaseId, x.Side, x.Repeat, null, true, 0, 0)
                ),
            },
            EvaluationEvents.CheckpointEvaluated x => s with
            {
                Checkpoints = s.Checkpoints.Add(x.Round),
            },
            EvaluationEvents.VerdictReached => s with
            {
                Verdict = true,
                Status = EvaluationStatus.Done,
            },
            EvaluationEvents.Cancelled => s with { Status = EvaluationStatus.Cancelled },
            EvaluationEvents.BudgetExhausted => s with { Exhausted = true },
            _ => s,
        };

    public static string StreamId(string id) => $"evaluation:{id}";

    public static string RunId(string caseId, Side side, int repeat) =>
        $"{caseId}:{(side == Side.Baseline ? "b" : "c")}:{repeat}";

    // Every run of a round: one repeat of every case on both sides.
    public IEnumerable<(string RunId, string CaseId, Side Side)> RoundRuns(int round) =>
        Cases.SelectMany(c =>
            new[] { Side.Baseline, Side.Candidate }.Select(side =>
                (RunId(c.CaseId, side, round), c.CaseId, side)
            )
        );

    public bool RoundDone(int round) => RoundRuns(round).All(r => Runs.ContainsKey(r.RunId));

    public bool Open => Status == EvaluationStatus.Running && !Verdict && !Exhausted;
}

// The rules of the evaluation stream: no run after a verdict or a cancel, a run ID once, spend
// never past the cap, a checkpoint once per round, and a verdict once.
public static class EvaluationDecider
{
    public const int MinimumCases = 10;

    public static IEnumerable<object> Request(Evaluation e, EvaluationEvents.Requested requested)
    {
        if (e.Exists)
            return [];
        if (requested.Cases.Count == 0)
            throw new DomainException("An evaluation needs at least one approved case.");
        if (requested.Repeats is < 1 or > 10)
            throw new DomainException("An evaluation runs 1 to 10 repeats.");
        if (requested.Delta is <= 0 or >= 0.5)
            throw new DomainException("The equivalence margin is between 0 and 0.5.");
        if (requested.Estimate.TotalUsd > requested.CapUsd)
            throw new DomainException(
                $"The estimate of {requested.Estimate.TotalUsd:0.00} USD is over this evaluation's cap of {requested.CapUsd:0.00} USD."
            );
        return [requested];
    }

    public static IEnumerable<object> Confirm(Evaluation e, string by)
    {
        Require(e);
        return e.Status switch
        {
            EvaluationStatus.AwaitingConfirmation => [new EvaluationEvents.Confirmed(by)],
            EvaluationStatus.Running or EvaluationStatus.Done => [],
            _ => throw new DomainException("A cancelled evaluation cannot be confirmed."),
        };
    }

    public static IEnumerable<object> CompleteRun(Evaluation e, EvaluationEvents.RunCompleted run)
    {
        RequireOpen(e);
        if (e.Runs.ContainsKey(run.RunId))
            return [];
        // A run's cost is money already spent, so it is always recorded. Rounds start only when
        // their estimate fits under the cap; a round that costs more than estimated stops the
        // evaluation here.
        if (e.SpentUsd + run.CostUsd > e.CapUsd)
            return [run, new EvaluationEvents.BudgetExhausted(e.SpentUsd + run.CostUsd)];
        return [run];
    }

    public static IEnumerable<object> FailRun(Evaluation e, EvaluationEvents.RunFailed run)
    {
        RequireOpen(e);
        return e.Runs.ContainsKey(run.RunId) ? [] : [run];
    }

    public static IEnumerable<object> Checkpoint(
        Evaluation e,
        EvaluationEvents.CheckpointEvaluated checkpoint
    )
    {
        RequireOpen(e);
        if (!e.RoundDone(checkpoint.Round))
            throw new DomainException($"Round {checkpoint.Round} still has runs to finish.");
        return e.Checkpoints.Contains(checkpoint.Round) ? [] : [checkpoint];
    }

    public static IEnumerable<object> Conclude(
        Evaluation e,
        EvaluationEvents.VerdictReached verdict
    )
    {
        Require(e);
        if (e.Verdict || e.Status == EvaluationStatus.Cancelled)
            return [];
        if (verdict.Verdict != Statistics.Verdict.Inconclusive && verdict.Cases < MinimumCases)
            throw new DomainException($"No verdict is given below {MinimumCases} cases.");
        return [verdict];
    }

    public static IEnumerable<object> Cancel(Evaluation e, string by, string reason)
    {
        Require(e);
        if (e.Status is EvaluationStatus.Done or EvaluationStatus.Cancelled)
            return [];
        return
        [
            new EvaluationEvents.Cancelled(
                by,
                string.IsNullOrWhiteSpace(reason) ? "cancelled" : reason.Trim()
            ),
        ];
    }

    private static void Require(Evaluation e)
    {
        if (!e.Exists)
            throw new NotFoundException("The evaluation does not exist.");
    }

    private static void RequireOpen(Evaluation e)
    {
        Require(e);
        if (!e.Open)
            throw new ConflictException(
                "The evaluation has a verdict or was cancelled; it records no more runs."
            );
    }
}
