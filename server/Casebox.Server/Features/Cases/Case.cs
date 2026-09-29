using System.Collections.Immutable;
using System.Security.Cryptography;
using System.Text;
using Deedbox;

namespace Casebox.Server.Features.Cases;

public enum CaseKind
{
    Capability,
    Regression,
    Steering,
}

public enum CaseScope
{
    Single,
    Split,
    Multi,
}

public enum RepoRole
{
    Sealed,
    Context,
}

public enum CaseStatus
{
    Mined,
    Validated,
    ValidationFailed,
    Approved,
    Rejected,
    Retired,
}

public enum CaseSplit
{
    Dev,
    HeldOut,
}

public enum ValidationFailure
{
    Build,
    NoFailToPass,
    FailToPassPassedAtBase,
    Flaky,
    TooSlow,
    FixDoesNotFail,
    ApplyFailed,
}

public enum RetireReason
{
    Environment,
    Drift,
    Manual,
}

// One repository of a case: its base commit, the merged commit when there is one, and whether the
// agent works on it (sealed) or only sees it at its merged state (context).
public sealed record CaseRepo(string Repo, string Base, string? Merged, RepoRole Role);

// A steering assertion: a forbidden file change, a command that must run before the agent says
// done, or a pattern the diff must or must not match.
public sealed record Assertion(string Kind, string? Path, string? Pattern);

public sealed record JudgeQuestion(string Question);

public static class CaseEvents
{
    public sealed record Mined(
        CaseKind Kind,
        string Workspace,
        string Source,
        string? WorkItem,
        CaseScope Scope,
        IReadOnlyList<CaseRepo> Repos,
        string RecipeHash,
        string HarnessHash,
        int Rank
    );

    public sealed record Validated(
        string Oracle,
        int FailToPass,
        int PassToPass,
        double Seconds,
        bool Drift,
        double Weight
    );

    public sealed record ValidationFailed(ValidationFailure Reason, string? Detail);

    // The instruction may hold the words of the issue's reporter or of the corrected person, so it
    // is their personal data: erasing them makes it unreadable, and the case needs a new one.
    public sealed record InstructionDrafted(
        [property: PersonalData] string? Text,
        [property: DataSubject] string Person,
        IReadOnlyList<string> Signatures,
        IReadOnlyList<Assertion> Assertions,
        IReadOnlyList<JudgeQuestion> Judge,
        string Model
    );

    public sealed record InstructionEdited(
        [property: PersonalData] string? Text,
        [property: DataSubject] string Person,
        string By
    );

    public sealed record AssertionsApproved(
        IReadOnlyList<Assertion> Assertions,
        IReadOnlyList<JudgeQuestion> Judge,
        string By
    );

    public sealed record Approved(string By);

    public sealed record Rejected(string Reason, string By);

    public sealed record SplitAssigned(CaseSplit Split);

    public sealed record Retired(RetireReason Reason);
}

public sealed record Case(
    bool Exists,
    CaseKind Kind,
    string Workspace,
    CaseStatus Status,
    bool Validated,
    bool HasInstruction,
    string? Person,
    bool AssertionsApproved,
    CaseSplit? Split
) : IState<Case>
{
    public static Case Initial { get; } =
        new(false, CaseKind.Capability, "", CaseStatus.Mined, false, false, null, false, null);

    public static Case Evolve(Case s, object e) =>
        e switch
        {
            CaseEvents.Mined x => s with
            {
                Exists = true,
                Kind = x.Kind,
                Workspace = x.Workspace,
                Status = CaseStatus.Mined,
            },
            CaseEvents.Validated => s with
            {
                Validated = true,
                Status = s.Status is CaseStatus.Approved ? s.Status : CaseStatus.Validated,
            },
            CaseEvents.ValidationFailed => s with
            {
                Validated = false,
                Status = CaseStatus.ValidationFailed,
            },
            CaseEvents.InstructionDrafted x => s with { HasInstruction = true, Person = x.Person },
            CaseEvents.InstructionEdited x => s with { HasInstruction = true, Person = x.Person },
            CaseEvents.AssertionsApproved => s with { AssertionsApproved = true },
            CaseEvents.Approved => s with { Status = CaseStatus.Approved },
            CaseEvents.Rejected => s with { Status = CaseStatus.Rejected },
            CaseEvents.SplitAssigned x => s with { Split = x.Split },
            CaseEvents.Retired => s with { Status = CaseStatus.Retired },
            // The instruction's author was erased: it reads as null now, and the case needs a new one.
            SubjectErased x when x.SubjectId == s.Person => s with { HasInstruction = false },
            _ => s,
        };

    // A case's ID comes from its source, so the same source is mined once.
    public static string IdFor(string orgId, string source) =>
        Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes($"{orgId}|{source}")))[
            ..20
        ];

    public static string StreamId(string id) => $"case:{id}";

    // Every fifth approved case of a workspace is held out (docs/specs/cases.md).
    public static CaseSplit SplitFor(int approvedBefore) =>
        (approvedBefore + 1) % 5 == 0 ? CaseSplit.HeldOut : CaseSplit.Dev;
}

// The rules of the case stream: approval needs a validated case and an instruction (and, for a
// steering case, approved assertions); retired is final.
public static class CaseDecider
{
    public static IEnumerable<object> Mine(Case c, CaseEvents.Mined mined)
    {
        if (c.Exists)
            return [];
        if (mined.Repos.Count == 0 || mined.Repos.All(r => r.Role != RepoRole.Sealed))
            throw new DomainException("A case needs at least one sealed repository.");
        return [mined];
    }

    public static IEnumerable<object> Validate(Case c, CaseEvents.Validated validated)
    {
        RequireOpen(c);
        if (validated.FailToPass == 0 && c.Kind != CaseKind.Steering)
            throw new DomainException(
                "A capability or regression case needs at least one fail-to-pass test."
            );
        return [validated];
    }

    public static IEnumerable<object> FailValidation(Case c, CaseEvents.ValidationFailed failed)
    {
        RequireOpen(c);
        return c.Status == CaseStatus.Approved
            ? [failed, new CaseEvents.Retired(RetireReason.Environment)]
            : [failed];
    }

    public static IEnumerable<object> DraftInstruction(
        Case c,
        CaseEvents.InstructionDrafted drafted
    )
    {
        RequireOpen(c);
        if (!c.Validated)
            throw new DomainException("Only a validated case gets an instruction.");
        if (string.IsNullOrWhiteSpace(drafted.Text))
            throw new DomainException("The instruction is empty.");
        return [drafted];
    }

    public static IEnumerable<object> EditInstruction(Case c, string text, string by)
    {
        RequireOpen(c);
        if (!c.Validated)
            throw new DomainException("Only a validated case gets an instruction.");
        if (string.IsNullOrWhiteSpace(text))
            throw new DomainException("The instruction is empty.");
        return [new CaseEvents.InstructionEdited(text.Trim(), c.Person ?? "system", by)];
    }

    public static IEnumerable<object> ApproveAssertions(
        Case c,
        IReadOnlyList<Assertion> assertions,
        IReadOnlyList<JudgeQuestion> judge,
        string by
    )
    {
        RequireOpen(c);
        if (c.Kind != CaseKind.Steering)
            throw new DomainException("Only a steering case has assertions.");
        if (assertions.Count == 0)
            throw new DomainException("A steering case needs at least one assertion.");
        foreach (var a in assertions)
            if (!Assertions.Kinds.Contains(a.Kind))
                throw new DomainException($"'{a.Kind}' is not an assertion kind.");
        if (judge.Count > 1)
            throw new DomainException("A case asks at most one judge question.");
        return [new CaseEvents.AssertionsApproved(assertions, judge, by)];
    }

    public static IEnumerable<object> Approve(Case c, string by, CaseSplit split)
    {
        RequireOpen(c);
        if (c.Status == CaseStatus.Approved)
            return [];
        if (c.Status == CaseStatus.Rejected)
            throw new DomainException("A rejected case is not approved.");
        if (!c.Validated)
            throw new DomainException("Only a validated case is approved.");
        if (!c.HasInstruction)
            throw new DomainException("The case needs an instruction first.");
        if (c.Kind == CaseKind.Steering && !c.AssertionsApproved)
            throw new DomainException("A steering case needs approved assertions first.");
        return c.Split is null
            ? [new CaseEvents.Approved(by), new CaseEvents.SplitAssigned(split)]
            : [new CaseEvents.Approved(by)];
    }

    public static IEnumerable<object> Reject(Case c, string reason, string by)
    {
        RequireOpen(c);
        if (string.IsNullOrWhiteSpace(reason))
            throw new DomainException("A rejection needs a reason.");
        return c.Status == CaseStatus.Rejected ? [] : [new CaseEvents.Rejected(reason.Trim(), by)];
    }

    public static IEnumerable<object> Retire(Case c, RetireReason reason)
    {
        Require(c);
        return c.Status == CaseStatus.Retired ? [] : [new CaseEvents.Retired(reason)];
    }

    private static void Require(Case c)
    {
        if (!c.Exists)
            throw new NotFoundException("The case does not exist.");
    }

    private static void RequireOpen(Case c)
    {
        Require(c);
        if (c.Status == CaseStatus.Retired)
            throw new DomainException("The case is retired.");
    }
}

public static class Assertions
{
    public static readonly HashSet<string> Kinds =
    [
        "forbidden_file",
        "command_before_done",
        "diff_must_match",
        "diff_must_not_match",
    ];
}
