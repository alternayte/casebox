using System.Collections.Immutable;
using Casebox.Server.Features.Evaluations;
using Deedbox;

namespace Casebox.Server.Features.Proposals;

public enum ProposalKind
{
    Edit,
    Removal,
}

public enum ProposalStatus
{
    Searching,
    Gating,
    GatePassed,
    GateFailed,
    GateInconclusive,
    PrOpened,
    Merged,
    Rejected,
}

// One edit of a candidate (docs/specs/self-evolution.md, The proposal stream).
public sealed record Edit(string Op, string File, string? Heading, string? Old, string? New);

// A candidate: its edits, the blob of its files ({ repo, files }) laid over the harness, the hash
// of its sorted edits, and the model's one-sentence reason.
public sealed record Candidate(
    int Index,
    IReadOnlyList<Edit> Edits,
    string Overrides,
    string ContentHash,
    string Rationale,
    IReadOnlyList<int>? MergedFrom = null
);

public sealed record GateCheck(string Name, bool Passed, string Evidence, string Detail);

public static class ProposalEvents
{
    public sealed record Drafted(
        string? Pattern,
        string Workspace,
        ProposalKind Kind,
        string Repo,
        string BaseCommit,
        IReadOnlyList<Candidate> Candidates,
        HarnessSpec Spec,
        IReadOnlyDictionary<string, Price> Prices,
        int Repeats,
        int BudgetRuns,
        IReadOnlyList<string> Batch,
        // The proposer run of casebox propose, whose inline worker may run the proposal's jobs.
        string? CiRun = null
    );

    public sealed record CandidateScored(
        int Index,
        string Evaluation,
        double Delta,
        double Lower,
        double Upper,
        IReadOnlyList<string> Wins,
        double? CostRatio,
        int Runs
    );

    // Two candidates that won on different cases, merged when they change different files.
    public sealed record CandidateMerged(Candidate Candidate);

    public sealed record GateRequested(int Index, string Evaluation);

    public sealed record GatePassed(string Evaluation, IReadOnlyList<GateCheck> Checks);

    public sealed record GateFailed(string Evaluation, IReadOnlyList<GateCheck> Checks);

    public sealed record GateInconclusive(
        string? Evaluation,
        IReadOnlyList<GateCheck> Checks,
        string Reason
    );

    public sealed record PrOpened(string Repo, int Number, string Branch, string Url);

    public sealed record Merged(DateTimeOffset At, string? Commit);

    public sealed record Rejected(string Reason, string By);

    public sealed record OutcomeObserved(
        double Before,
        double After,
        int BeforeN,
        int AfterN,
        bool Fell
    );
}

public sealed record Proposal(
    bool Exists,
    ProposalStatus Status,
    ProposalEvents.Drafted? Draft,
    ImmutableList<Candidate> Candidates,
    ImmutableDictionary<int, ProposalEvents.CandidateScored> Scores,
    int? GateIndex,
    string? GateEvaluation,
    ProposalEvents.PrOpened? Pr,
    DateTimeOffset? MergedAt,
    bool OutcomeObserved,
    IReadOnlyList<GateCheck>? GateChecks = null
) : IState<Proposal>
{
    public static Proposal Initial { get; } =
        new(
            false,
            ProposalStatus.Searching,
            null,
            [],
            ImmutableDictionary<int, ProposalEvents.CandidateScored>.Empty,
            null,
            null,
            null,
            null,
            false
        );

    public static Proposal Evolve(Proposal s, object e) =>
        e switch
        {
            ProposalEvents.Drafted x => s with
            {
                Exists = true,
                Draft = x,
                Candidates = [.. x.Candidates],
            },
            ProposalEvents.CandidateScored x => s with { Scores = s.Scores.SetItem(x.Index, x) },
            ProposalEvents.CandidateMerged x => s with
            {
                Candidates = s.Candidates.Add(x.Candidate),
            },
            ProposalEvents.GateRequested x => s with
            {
                Status = ProposalStatus.Gating,
                GateIndex = x.Index,
                GateEvaluation = x.Evaluation,
            },
            ProposalEvents.GatePassed x => s with
            {
                Status = ProposalStatus.GatePassed,
                GateChecks = x.Checks,
            },
            ProposalEvents.GateFailed x => s with
            {
                Status = ProposalStatus.GateFailed,
                GateChecks = x.Checks,
            },
            ProposalEvents.GateInconclusive x => s with
            {
                Status = ProposalStatus.GateInconclusive,
                GateChecks = x.Checks,
            },
            ProposalEvents.PrOpened x => s with { Status = ProposalStatus.PrOpened, Pr = x },
            ProposalEvents.Merged x => s with { Status = ProposalStatus.Merged, MergedAt = x.At },
            ProposalEvents.Rejected => s with { Status = ProposalStatus.Rejected },
            ProposalEvents.OutcomeObserved => s with { OutcomeObserved = true },
            _ => s,
        };

    public static string StreamId(string id) => $"proposal:{id}";

    public Candidate? Gated =>
        GateIndex is { } i ? Candidates.FirstOrDefault(c => c.Index == i) : null;

    // Still working or waiting for a person: another proposal for its pattern would compete.
    public bool InFlight =>
        Status
            is ProposalStatus.Searching
                or ProposalStatus.Gating
                or ProposalStatus.GatePassed
                or ProposalStatus.PrOpened;
}

// The rules of the proposal stream: a PR only after gate_passed and once; a gate once, after a
// score; a rejection needs a reason and never follows a merge; an outcome only after a merge.
public static class ProposalDecider
{
    public const int MaxCandidates = 5;
    public const int MaxEdits = 3;

    public static IEnumerable<object> Draft(Proposal p, ProposalEvents.Drafted drafted)
    {
        if (p.Exists)
            return [];
        if (drafted.Candidates.Count is < 1 or > MaxCandidates)
            throw new DomainException($"A proposal has 1 to {MaxCandidates} candidates.");
        foreach (var c in drafted.Candidates)
            if (c.Edits.Count is < 1 or > MaxEdits)
                throw new DomainException($"A candidate has 1 to {MaxEdits} edits.");
        if (drafted.Kind == ProposalKind.Edit && drafted.Pattern is null)
            throw new DomainException("An edit proposal names its pattern.");
        return [drafted];
    }

    public static IEnumerable<object> Score(Proposal p, ProposalEvents.CandidateScored scored)
    {
        Require(p);
        if (p.Candidates.All(c => c.Index != scored.Index))
            throw new DomainException($"The proposal has no candidate {scored.Index}.");
        return p.Scores.ContainsKey(scored.Index) || p.Status != ProposalStatus.Searching
            ? []
            : [scored];
    }

    public static IEnumerable<object> Merge(Proposal p, Candidate merged)
    {
        Require(p);
        if (merged.Edits.Count > MaxEdits)
            throw new DomainException($"A merged candidate has at most {MaxEdits} edits.");
        return
            p.Candidates.Any(c => c.Index == merged.Index) || p.Status != ProposalStatus.Searching
            ? []
            : [new ProposalEvents.CandidateMerged(merged)];
    }

    public static IEnumerable<object> RequestGate(Proposal p, int index, string evaluation)
    {
        Require(p);
        if (!p.Scores.ContainsKey(index))
            throw new DomainException("Only a scored candidate goes to the gate.");
        return p.Status == ProposalStatus.Searching
            ? [new ProposalEvents.GateRequested(index, evaluation)]
            : [];
    }

    public static IEnumerable<object> Conclude(Proposal p, object outcome)
    {
        Require(p);
        if (
            outcome
            is not (
                ProposalEvents.GatePassed
                or ProposalEvents.GateFailed
                or ProposalEvents.GateInconclusive
            )
        )
            throw new ArgumentException("Not a gate outcome.", nameof(outcome));
        // A concluded proposal takes no second outcome: the verdict can be delivered again.
        if (p.Status is not (ProposalStatus.Searching or ProposalStatus.Gating))
            return [];
        if (
            outcome is ProposalEvents.GatePassed or ProposalEvents.GateFailed
            && p.Status != ProposalStatus.Gating
        )
            throw new DomainException("A gate verdict needs a gate request.");
        return [outcome];
    }

    public static IEnumerable<object> OpenPr(Proposal p, ProposalEvents.PrOpened pr)
    {
        Require(p);
        if (p.Pr is not null)
            return [];
        if (p.Status != ProposalStatus.GatePassed)
            throw new DomainException("A pull request opens only after the gate passed.");
        return [pr];
    }

    public static IEnumerable<object> Merge(Proposal p, DateTimeOffset at, string? commit)
    {
        Require(p);
        if (p.Status == ProposalStatus.Merged)
            return [];
        if (p.Status != ProposalStatus.PrOpened)
            throw new DomainException("Only an opened pull request merges.");
        return [new ProposalEvents.Merged(at, commit)];
    }

    public static IEnumerable<object> Reject(Proposal p, string reason, string by)
    {
        Require(p);
        if (string.IsNullOrWhiteSpace(reason))
            throw new DomainException("A rejection needs a reason.");
        if (p.Status == ProposalStatus.Merged)
            throw new DomainException("A merged proposal is undone by reverting its pull request.");
        return p.Status == ProposalStatus.Rejected
            ? []
            : [new ProposalEvents.Rejected(reason.Trim(), by)];
    }

    public static IEnumerable<object> Observe(Proposal p, ProposalEvents.OutcomeObserved outcome)
    {
        Require(p);
        if (p.Status != ProposalStatus.Merged)
            throw new DomainException("An outcome is observed only after the merge.");
        return p.OutcomeObserved ? [] : [outcome];
    }

    private static void Require(Proposal p)
    {
        if (!p.Exists)
            throw new NotFoundException("The proposal does not exist.");
    }
}
