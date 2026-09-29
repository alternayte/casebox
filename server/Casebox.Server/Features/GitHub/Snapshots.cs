namespace Casebox.Server.Features.GitHub;

// A person as a poll result carries them: a subject ID, or a bot's login. Never an identity.
public sealed record PersonRef(string Token, bool Bot, bool Mapped);

// Line ranges a pull request removed or changed, in the base version of each file: what the fix
// detection blames. The diff itself is not kept.
public sealed record ChangedFile(string Path, int Additions, int Deletions, IReadOnlyList<int[]> BaseRanges);

public sealed record PrCommit(string Sha, string? Message, PersonRef? Author, DateTimeOffset At, bool AgentCoAuthor);

public sealed record PrReview(long Id, string State, PersonRef? Author, DateTimeOffset? At, string? Body);

public sealed record PrReviewComment(long Id, long? ReviewId, long? InReplyTo, string Path, int? Line, int? OriginalLine, string? CommitSha, PersonRef? Author, DateTimeOffset At, string? Body);

public sealed record PrCheck(string Name, string? Conclusion, string HeadSha, DateTimeOffset? CompletedAt);

// A pull request a fix blames lines to, with how many lines.
public sealed record BlamedPr(string Repo, int Number, int Lines);

public sealed record PullRequestSnapshot(
    string Repo,
    int Number,
    string? Title,
    string? Body,
    string State,
    bool Draft,
    string HeadRef,
    string BaseRef,
    string HeadSha,
    string BaseSha,
    PersonRef? Author,
    DateTimeOffset CreatedAt,
    DateTimeOffset UpdatedAt,
    DateTimeOffset? MergedAt,
    string? MergeSha,
    IReadOnlyList<string> Labels,
    IReadOnlyList<ChangedFile> Files,
    IReadOnlyList<PrCommit> Commits,
    IReadOnlyList<PrReview> Reviews,
    IReadOnlyList<PrReviewComment> ReviewComments,
    IReadOnlyList<PrCheck> Checks,
    IReadOnlyList<BlamedPr> Blamed,
    int? Reverts);

public sealed record IssueSnapshot(string Repo, int Number, string? Title, string? Body, string State, IReadOnlyList<string> Labels, PersonRef? Assignee, DateTimeOffset CreatedAt, DateTimeOffset? ClosedAt, DateTimeOffset UpdatedAt);

// A commit on the default branch that reverts another commit, pushed without a pull request.
public sealed record RevertCommit(string Repo, string Sha, string RevertedSha, int? RevertedPr, DateTimeOffset At);
