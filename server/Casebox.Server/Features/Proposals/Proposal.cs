using Deedbox;

namespace Casebox.Server.Features.Proposals;

// What a proposal changes (docs/specs/simple-evolution.md). Casebox writes the first three; a code
// note is carried out by the person's own agent.
public enum ProposalKind
{
    HarnessEdit,
    Skill,
    Mcp,
    CodeNote,
}

public enum ProposalStatus
{
    Open,
    Approved,
    Rejected,
    Applied,
}

// Private: its own untracked file on the person's machine. Commit: an edit of the shared files.
public enum ApplyMode
{
    Private,
    Commit,
}

// One edit, applied by `casebox apply` on the working tree with the same rules the draft used.
public sealed record Edit(
    string Op,
    string File,
    string? Heading = null,
    string? Old = null,
    string? New = null
);

// A changed file as the draft saw it at its base commit, for review: before is null for a new file.
public sealed record FilePreview(string Path, string? Before, string After);

// What a code note asks the person's agent to do, and the prompt that asks it.
public sealed record CodeNote(string What, string Why, string Prompt);

public static class ProposalEvents
{
    public sealed record Drafted(
        string Pattern,
        string Workspace,
        string Repo,
        ProposalKind Kind,
        string Title,
        string Rationale,
        IReadOnlyList<Edit> Edits,
        IReadOnlyList<FilePreview> Preview,
        CodeNote? Note,
        string ContentHash,
        string BaseCommit,
        string? Model
    );

    public sealed record Approved(string By);

    public sealed record Rejected(string Reason, string By);

    public sealed record Applied(ApplyMode Mode, string By, DateTimeOffset At);

    // The pattern's corrections per 100 sessions of the workspace, 30 days before and after the
    // first apply (observational).
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
    ApplyMode? AppliedMode,
    DateTimeOffset? AppliedAt,
    bool OutcomeObserved
) : IState<Proposal>
{
    public static Proposal Initial { get; } =
        new(false, ProposalStatus.Open, null, null, null, false);

    public static Proposal Evolve(Proposal s, object e) =>
        e switch
        {
            ProposalEvents.Drafted x => s with { Exists = true, Draft = x },
            ProposalEvents.Approved => s with { Status = ProposalStatus.Approved },
            ProposalEvents.Rejected => s with { Status = ProposalStatus.Rejected },
            ProposalEvents.Applied x => s with
            {
                Status = ProposalStatus.Applied,
                AppliedMode = x.Mode,
                AppliedAt = s.AppliedAt ?? x.At,
            },
            ProposalEvents.OutcomeObserved => s with { OutcomeObserved = true },
            _ => s,
        };

    public static string StreamId(string id) => $"proposal:{id}";

    // Waiting for a person: open, or approved and not yet applied. At most MaxOpen per workspace.
    public bool Pending => Status is ProposalStatus.Open or ProposalStatus.Approved;
}

// The rules of the proposal stream: drafted once; approved or rejected while open; a rejection
// needs a reason; applied only after approval, privately first or straight to a commit, and once
// committed never private again; an outcome only after an apply, once.
public static class ProposalDecider
{
    public const int MaxEdits = 3;

    public static IEnumerable<object> Draft(Proposal p, ProposalEvents.Drafted drafted)
    {
        if (p.Exists)
            return [];
        if (string.IsNullOrWhiteSpace(drafted.Title))
            throw new DomainException("A proposal needs a title.");
        if (drafted.Kind == ProposalKind.CodeNote)
        {
            if (drafted.Note is null || drafted.Edits.Count > 0)
                throw new DomainException("A code note has its note and no edits.");
        }
        else if (drafted.Edits.Count is < 1 or > MaxEdits || drafted.Note is not null)
            throw new DomainException($"A proposal makes 1 to {MaxEdits} edits.");
        return [drafted];
    }

    public static IEnumerable<object> Approve(Proposal p, string by)
    {
        Require(p);
        return p.Status switch
        {
            ProposalStatus.Open => [new ProposalEvents.Approved(by)],
            ProposalStatus.Approved or ProposalStatus.Applied => [],
            _ => throw new DomainException("A rejected proposal cannot be approved."),
        };
    }

    public static IEnumerable<object> Reject(Proposal p, string reason, string by)
    {
        Require(p);
        if (string.IsNullOrWhiteSpace(reason))
            throw new DomainException("A rejection needs a reason; the next draft reads it.");
        return p.Status switch
        {
            ProposalStatus.Rejected => [],
            ProposalStatus.Applied => throw new DomainException(
                "An applied proposal is undone by reverting its change."
            ),
            _ => [new ProposalEvents.Rejected(reason.Trim(), by)],
        };
    }

    public static IEnumerable<object> Apply(
        Proposal p,
        ApplyMode mode,
        string by,
        DateTimeOffset at
    )
    {
        Require(p);
        if (p.Status is ProposalStatus.Open or ProposalStatus.Rejected)
            throw new DomainException("Only an approved proposal is applied.");
        if (p.Draft!.Kind == ProposalKind.CodeNote && mode == ApplyMode.Private)
            throw new DomainException("A code note changes code, which cannot stay private.");
        if (p.AppliedMode == mode || p.AppliedMode == ApplyMode.Commit)
            return [];
        return [new ProposalEvents.Applied(mode, by, at)];
    }

    public static IEnumerable<object> Observe(Proposal p, ProposalEvents.OutcomeObserved outcome)
    {
        Require(p);
        if (p.Status != ProposalStatus.Applied)
            throw new DomainException("An outcome is observed only after an apply.");
        return p.OutcomeObserved ? [] : [outcome];
    }

    private static void Require(Proposal p)
    {
        if (!p.Exists)
            throw new NotFoundException("The proposal does not exist.");
    }
}
