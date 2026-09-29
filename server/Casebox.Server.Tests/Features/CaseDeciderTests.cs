using Casebox.Server.Features.Cases;
using Deedbox;
using Deedbox.Testing;

namespace Casebox.Server.Tests.Features;

public sealed class CaseDeciderTests
{
    private static readonly CaseEvents.Mined Mined = new(
        CaseKind.Capability,
        "payments",
        "pr:github.com/acme/api#7",
        "wi:jira:PAY-1",
        CaseScope.Single,
        [new CaseRepo("github.com/acme/api", "base", "merged", RepoRole.Sealed)],
        "recipe",
        "harness",
        10
    );

    private static readonly CaseEvents.Validated Validated = new("oracle", 2, 30, 42, false, 1);

    private static readonly CaseEvents.InstructionDrafted Drafted = new(
        "Add retries to the payment client.",
        "person:aaaaaaaaaaaaaaaaaaaaaaaaaa",
        [],
        [],
        [],
        "m"
    );

    [Fact]
    public void A_case_is_mined_once() =>
        Decider.Given<Case>(Mined).When(c => CaseDecider.Mine(c, Mined)).ThenNothing();

    [Fact]
    public void A_case_needs_a_sealed_repository() =>
        Decider
            .Given<Case>()
            .When(c =>
                CaseDecider.Mine(
                    c,
                    Mined with
                    {
                        Repos = [new CaseRepo("github.com/acme/api", "b", null, RepoRole.Context)],
                    }
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void A_capability_case_needs_a_fail_to_pass_test() =>
        Decider
            .Given<Case>(Mined)
            .When(c => CaseDecider.Validate(c, Validated with { FailToPass = 0 }))
            .ThenThrows<DomainException>();

    [Fact]
    public void Approval_needs_an_instruction() =>
        Decider
            .Given<Case>(Mined, Validated)
            .When(c => CaseDecider.Approve(c, "acct", CaseSplit.Dev))
            .ThenThrows<DomainException>();

    [Fact]
    public void Approval_assigns_the_split_once() =>
        Decider
            .Given<Case>(Mined, Validated, Drafted)
            .When(c => CaseDecider.Approve(c, "acct", CaseSplit.HeldOut))
            .Then(new CaseEvents.Approved("acct"), new CaseEvents.SplitAssigned(CaseSplit.HeldOut));

    [Fact]
    public void A_steering_case_needs_approved_assertions() =>
        Decider
            .Given<Case>(
                Mined with
                {
                    Kind = CaseKind.Steering,
                },
                Validated with
                {
                    FailToPass = 0,
                },
                Drafted
            )
            .When(c => CaseDecider.Approve(c, "acct", CaseSplit.Dev))
            .ThenThrows<DomainException>();

    [Fact]
    public void An_erased_instruction_must_be_written_again_before_approval() =>
        Decider
            .Given<Case>(
                Mined,
                Validated,
                Drafted,
                new SubjectErased("person:aaaaaaaaaaaaaaaaaaaaaaaaaa")
            )
            .When(c => CaseDecider.Approve(c, "acct", CaseSplit.Dev))
            .ThenThrows<DomainException>();

    [Fact]
    public void An_approved_case_whose_environment_no_longer_builds_is_retired() =>
        Decider
            .Given<Case>(Mined, Validated, Drafted, new CaseEvents.Approved("acct"))
            .When(c =>
                CaseDecider.FailValidation(
                    c,
                    new CaseEvents.ValidationFailed(ValidationFailure.Build, "x")
                )
            )
            .Then(
                new CaseEvents.ValidationFailed(ValidationFailure.Build, "x"),
                new CaseEvents.Retired(RetireReason.Environment)
            );

    [Fact]
    public void Retired_is_final() =>
        Decider
            .Given<Case>(Mined, new CaseEvents.Retired(RetireReason.Manual))
            .When(c => CaseDecider.Validate(c, Validated))
            .ThenThrows<DomainException>();

    [Fact]
    public void Every_fifth_approved_case_is_held_out() =>
        Assert.Equal(
            [
                CaseSplit.Dev,
                CaseSplit.Dev,
                CaseSplit.Dev,
                CaseSplit.Dev,
                CaseSplit.HeldOut,
                CaseSplit.Dev,
            ],
            Enumerable.Range(0, 6).Select(Case.SplitFor)
        );
}
