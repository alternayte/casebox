using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Proposals;
using Deedbox;
using Deedbox.Testing;

namespace Casebox.Server.Tests.Features;

// The rules of step 12's streams (docs/specs/self-evolution.md).
public sealed class ProposalDeciderTests
{
    private static readonly PatternEvents.Detected Detected = new(
        "payments",
        "broke_convention",
        null,
        "instruction",
        "api",
        "Mocks the database in integration tests",
        "The agent mocks the database where the team uses the real one.",
        ["r1", "r2", "r3"],
        false
    );

    private static readonly HarnessSpec Spec = new(
        "claude-code",
        "2.1.284",
        "claude-sonnet-5-20260801",
        null,
        "HEAD",
        new AgentSettings(null, null, null),
        null
    );

    private static readonly Candidate First = new(
        0,
        [new Edit("add_bullet", "AGENTS.md", "Testing", null, "Use the real database.")],
        "blob",
        "hash",
        "Says what to do."
    );

    private static readonly ProposalEvents.Drafted Drafted = new(
        "p1",
        "payments",
        ProposalKind.Edit,
        "github.com/acme/api",
        "base",
        [First],
        Spec,
        new Dictionary<string, Price>(),
        3,
        40,
        ["c1"]
    );

    private static readonly ProposalEvents.CandidateScored Scored = new(
        0,
        "e1",
        0.2,
        0.1,
        0.3,
        ["c1"],
        0.9,
        8
    );

    [Fact]
    public void A_dismissal_needs_a_reason_and_stops_new_corrections() =>
        Decider
            .Given<Pattern>(Detected)
            .When(p => PatternDecider.Dismiss(p, "account:a", " "))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_dismissed_pattern_takes_no_more_corrections() =>
        Decider
            .Given<Pattern>(Detected, new PatternEvents.Dismissed("account:a", "not ours"))
            .When(p => PatternDecider.AddCorrections(p, ["r4"]))
            .ThenNothing();

    [Fact]
    public void Only_new_refs_are_added() =>
        Decider
            .Given<Pattern>(Detected)
            .When(p => PatternDecider.AddCorrections(p, ["r2", "r4"]))
            .Then(new PatternEvents.CorrectionsAdded(["r4"]));

    [Fact]
    public void A_pattern_is_resolved_only_when_its_rate_fell() =>
        Decider
            .Given<Pattern>(Detected)
            .When(p => PatternDecider.Resolve(p, "p1", 2.0, 2.5))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_pull_request_opens_only_after_the_gate_passed() =>
        Decider
            .Given<Proposal>(Drafted, Scored)
            .When(p =>
                ProposalDecider.OpenPr(
                    p,
                    new ProposalEvents.PrOpened("github.com/acme/api", 5, "b", "u")
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void A_pull_request_opens_once() =>
        Decider
            .Given<Proposal>(
                Drafted,
                Scored,
                new ProposalEvents.GateRequested(0, "g"),
                new ProposalEvents.GatePassed("g", []),
                new ProposalEvents.PrOpened("github.com/acme/api", 5, "b", "u")
            )
            .When(p =>
                ProposalDecider.OpenPr(
                    p,
                    new ProposalEvents.PrOpened("github.com/acme/api", 6, "b", "u")
                )
            )
            .ThenNothing();

    [Fact]
    public void A_gate_verdict_delivered_again_appends_nothing() =>
        Decider
            .Given<Proposal>(
                Drafted,
                Scored,
                new ProposalEvents.GateRequested(0, "g"),
                new ProposalEvents.GatePassed("g", []),
                new ProposalEvents.PrOpened("github.com/acme/api", 5, "b", "u")
            )
            .When(p => ProposalDecider.Conclude(p, new ProposalEvents.GatePassed("g", [])))
            .ThenNothing();

    [Fact]
    public void A_merged_proposal_is_not_rejected() =>
        Decider
            .Given<Proposal>(
                Drafted,
                Scored,
                new ProposalEvents.GateRequested(0, "g"),
                new ProposalEvents.GatePassed("g", []),
                new ProposalEvents.PrOpened("github.com/acme/api", 5, "b", "u"),
                new ProposalEvents.Merged(DateTimeOffset.UnixEpoch, null)
            )
            .When(p => ProposalDecider.Reject(p, "changed my mind", "account:a"))
            .ThenThrows<DomainException>();

    [Fact]
    public void Only_a_scored_candidate_goes_to_the_gate() =>
        Decider
            .Given<Proposal>(Drafted)
            .When(p => ProposalDecider.RequestGate(p, 0, "g"))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_candidate_has_at_most_three_edits() =>
        Decider
            .Given<Proposal>()
            .When(p =>
                ProposalDecider.Draft(
                    p,
                    Drafted with
                    {
                        Candidates =
                        [
                            First with
                            {
                                Edits = [.. Enumerable.Repeat(First.Edits[0], 4)],
                            },
                        ],
                    }
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void The_held_out_budget_caps_the_queries_of_a_rotation() =>
        Decider
            .Given<Suite>([
                .. Enumerable
                    .Range(0, Suite.DefaultBudget)
                    .Select(i => new SuiteEvents.HoldoutQueried($"p{i}", $"e{i}", 1)),
            ])
            .When(s => SuiteDecider.Query(s, "p10", "e10", Suite.DefaultBudget))
            .ThenThrows<DomainException>();

    [Fact]
    public void The_suite_rotates_only_when_its_budget_is_spent() =>
        Decider
            .Given<Suite>(new SuiteEvents.HoldoutQueried("p0", "e0", 1))
            .When(s =>
                SuiteDecider.Rotate(s, ["a"], ["b"], Suite.DefaultBudget, DateTimeOffset.UnixEpoch)
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void The_best_candidate_has_the_highest_delta_above_zero()
    {
        var p = new object[]
        {
            Drafted with
            {
                Candidates = [First, First with { Index = 1 }, First with { Index = 2 }],
            },
            Scored with
            {
                Delta = 0.1,
            },
            Scored with
            {
                Index = 1,
                Delta = 0.3,
            },
            Scored with
            {
                Index = 2,
                Delta = -0.1,
            },
        }.Aggregate(Proposal.Initial, Proposal.Evolve);
        Assert.Equal(1, ProposalSteps.Best(p));
        Assert.Null(ProposalSteps.Best(p with { Scores = p.Scores.RemoveRange([0, 1]) }));
    }
}
