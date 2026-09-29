using System.Collections.Immutable;
using Deedbox;

namespace Casebox.Server.Features.WorkItems;

public enum LinkSource { Explicit, Branch, PullRequest, SameBranch, SameWorkItem }

public enum Confidence { Low, High, Explicit }

public enum SnapshotReason { FirstLinked, WorkStarted, TextChanged, Resolved }

// A work item's text and fields at one moment. Text is redacted and its identities are tokens.
public sealed record Snapshot(string Title, string? Description, string? Type, IReadOnlyList<string> Labels, string? Status, string? Assignee, bool Resolved, string Hash);

public static class WorkItemEvents
{
    public sealed record Imported(string Provider, string Key, string? Repo, DateTimeOffset CreatedAt);

    public sealed record SnapshotCaptured(Snapshot Snapshot, SnapshotReason Reason);

    public sealed record SessionLinked(string SessionId, LinkSource Source, Confidence Confidence);

    public sealed record SessionReassigned(string SessionId, string ToWorkItem);

    public sealed record PrLinked(string Repo, int Number, LinkSource Source, Confidence Confidence);

    public sealed record ReviewObserved(string Repo, int Number, long ReviewId, string State, string Reviewer, int Comments, DateTimeOffset At);

    public sealed record CiObserved(string Repo, int Number, string Sha, string Check, string Conclusion, DateTimeOffset At);

    public sealed record Merged(string Repo, int Number, string Sha, DateTimeOffset At);

    public sealed record RevertObserved(string Repo, int Number, string RevertedBy, DateTimeOffset At);

    public sealed record FixObserved(string Repo, int Number, string FixedBy, int Lines, LinkSource Source, DateTimeOffset At);
}

public sealed record WorkItem(
    bool Exists,
    string Provider,
    string Key,
    string? SnapshotHash,
    bool Resolved,
    bool Started,
    ImmutableDictionary<string, Confidence> Sessions,
    ImmutableHashSet<string> PullRequests,
    ImmutableHashSet<string> MergedPullRequests,
    ImmutableHashSet<string> Observed) : IState<WorkItem>
{
    public static WorkItem Initial { get; } = new(false, "", "", null, false, false,
        ImmutableDictionary<string, Confidence>.Empty, ImmutableHashSet<string>.Empty, ImmutableHashSet<string>.Empty, ImmutableHashSet<string>.Empty);

    public static WorkItem Evolve(WorkItem s, object e) => e switch
    {
        WorkItemEvents.Imported x => s with { Exists = true, Provider = x.Provider, Key = x.Key },
        WorkItemEvents.SnapshotCaptured x => s with
        {
            SnapshotHash = x.Snapshot.Hash,
            Resolved = x.Snapshot.Resolved,
            Started = s.Started || x.Reason == SnapshotReason.WorkStarted,
        },
        WorkItemEvents.SessionLinked x => s with { Sessions = s.Sessions.SetItem(x.SessionId, x.Confidence), Started = true },
        WorkItemEvents.SessionReassigned x => s with { Sessions = s.Sessions.Remove(x.SessionId) },
        WorkItemEvents.PrLinked x => s with { PullRequests = s.PullRequests.Add(Pr(x.Repo, x.Number)), Started = true },
        WorkItemEvents.Merged x => s with { MergedPullRequests = s.MergedPullRequests.Add(Pr(x.Repo, x.Number)) },
        WorkItemEvents.ReviewObserved x => s with { Observed = s.Observed.Add($"review:{x.Repo}#{x.Number}:{x.ReviewId}") },
        WorkItemEvents.CiObserved x => s with { Observed = s.Observed.Add($"ci:{x.Repo}#{x.Number}:{x.Sha}:{x.Check}:{x.Conclusion}") },
        WorkItemEvents.RevertObserved x => s with { Observed = s.Observed.Add($"revert:{x.Repo}#{x.Number}:{x.RevertedBy}") },
        WorkItemEvents.FixObserved x => s with { Observed = s.Observed.Add($"fix:{x.Repo}#{x.Number}:{x.FixedBy}") },
        _ => s,
    };

    public static string Pr(string repo, int number) => $"{repo}#{number}";

    public static string JiraStream(string key) => $"wi:jira:{key}";

    public static string GitHubStream(string repo, int number) => $"wi:github:{repo}#{number}";
}

// The rules the work_item stream enforces: every link has a source and a confidence, and
// after-merge observations need the merge first. Repeated observations append nothing.
public static class WorkItemDecider
{
    public static IEnumerable<object> Import(WorkItem w, string provider, string key, string? repo, DateTimeOffset createdAt, Snapshot snapshot)
    {
        if (w.Exists) return Capture(w, snapshot);
        return [new WorkItemEvents.Imported(provider, key, repo, createdAt), new WorkItemEvents.SnapshotCaptured(snapshot, SnapshotReason.FirstLinked)];
    }

    // A new snapshot when the text changed (by hash) or the item was resolved.
    public static IEnumerable<object> Capture(WorkItem w, Snapshot snapshot)
    {
        Require(w);
        if (snapshot.Resolved && !w.Resolved) return [new WorkItemEvents.SnapshotCaptured(snapshot, SnapshotReason.Resolved)];
        return snapshot.Hash == w.SnapshotHash ? [] : [new WorkItemEvents.SnapshotCaptured(snapshot, SnapshotReason.TextChanged)];
    }

    public static IEnumerable<object> LinkSession(WorkItem w, string sessionId, LinkSource source, Confidence confidence, Snapshot? current)
    {
        Require(w);
        if (confidence == Confidence.Low) throw new DomainException("A low-confidence link is a suggestion, never a link.");
        if (w.Sessions.TryGetValue(sessionId, out var existing) && existing >= confidence) return [];
        List<object> events = [new WorkItemEvents.SessionLinked(sessionId, source, confidence)];
        if (!w.Started && current is not null) events.Add(new WorkItemEvents.SnapshotCaptured(current, SnapshotReason.WorkStarted));
        return events;
    }

    public static IEnumerable<object> Reassign(WorkItem w, string sessionId, string toWorkItem)
    {
        Require(w);
        return w.Sessions.ContainsKey(sessionId) ? [new WorkItemEvents.SessionReassigned(sessionId, toWorkItem)] : [];
    }

    public static IEnumerable<object> LinkPr(WorkItem w, string repo, int number, LinkSource source, Confidence confidence, Snapshot? current)
    {
        Require(w);
        if (confidence == Confidence.Low) throw new DomainException("A low-confidence link is a suggestion, never a link.");
        if (w.PullRequests.Contains(WorkItem.Pr(repo, number))) return [];
        List<object> events = [new WorkItemEvents.PrLinked(repo, number, source, confidence)];
        if (!w.Started && current is not null) events.Add(new WorkItemEvents.SnapshotCaptured(current, SnapshotReason.WorkStarted));
        return events;
    }

    public static IEnumerable<object> ObserveReview(WorkItem w, WorkItemEvents.ReviewObserved review) =>
        RequirePr(w, review.Repo, review.Number).Observed.Contains($"review:{review.Repo}#{review.Number}:{review.ReviewId}") ? [] : [review];

    public static IEnumerable<object> ObserveCi(WorkItem w, WorkItemEvents.CiObserved ci) =>
        RequirePr(w, ci.Repo, ci.Number).Observed.Contains($"ci:{ci.Repo}#{ci.Number}:{ci.Sha}:{ci.Check}:{ci.Conclusion}") ? [] : [ci];

    public static IEnumerable<object> Merge(WorkItem w, WorkItemEvents.Merged merged) =>
        RequirePr(w, merged.Repo, merged.Number).MergedPullRequests.Contains(WorkItem.Pr(merged.Repo, merged.Number)) ? [] : [merged];

    public static IEnumerable<object> ObserveRevert(WorkItem w, WorkItemEvents.RevertObserved revert)
    {
        RequireMerged(w, revert.Repo, revert.Number);
        return w.Observed.Contains($"revert:{revert.Repo}#{revert.Number}:{revert.RevertedBy}") ? [] : [revert];
    }

    public static IEnumerable<object> ObserveFix(WorkItem w, WorkItemEvents.FixObserved fix)
    {
        RequireMerged(w, fix.Repo, fix.Number);
        return w.Observed.Contains($"fix:{fix.Repo}#{fix.Number}:{fix.FixedBy}") ? [] : [fix];
    }

    private static void Require(WorkItem w)
    {
        if (!w.Exists) throw new NotFoundException("The work item has not been imported.");
    }

    private static WorkItem RequirePr(WorkItem w, string repo, int number)
    {
        Require(w);
        if (!w.PullRequests.Contains(WorkItem.Pr(repo, number)))
            throw new DomainException($"Pull request {repo}#{number} is not linked to this work item.");
        return w;
    }

    private static void RequireMerged(WorkItem w, string repo, int number)
    {
        Require(w);
        if (!w.MergedPullRequests.Contains(WorkItem.Pr(repo, number)))
            throw new DomainException($"An after-merge observation needs {repo}#{number} to be merged first.");
    }
}
