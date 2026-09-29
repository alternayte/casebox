using Casebox.Server.Features.WorkItems;
using Deedbox.Testing;

namespace Casebox.Server.Tests.Features;

public sealed class WorkItemDeciderTests
{
    private static readonly DateTimeOffset At = new(2026, 9, 29, 10, 0, 0, TimeSpan.Zero);
    private static readonly Snapshot Open = new("Fix login", "Users cannot log in", "Bug", ["auth"], "Open", "person:aaaa", false, "h1");
    private static readonly WorkItemEvents.Imported Imported = new("jira", "PAY-1", null, At);
    private static readonly WorkItemEvents.SnapshotCaptured First = new(Open, SnapshotReason.FirstLinked);
    private static readonly WorkItemEvents.PrLinked Pr = new("github.com/acme/app", 7, LinkSource.Branch, Confidence.High);

    [Fact]
    public void Importing_captures_the_first_snapshot() =>
        Decider.Given<WorkItem>().When(w => WorkItemDecider.Import(w, "jira", "PAY-1", null, At, Open)).Then(Imported, First);

    [Fact]
    public void An_unchanged_snapshot_appends_nothing() =>
        Decider.Given<WorkItem>(Imported, First).When(w => WorkItemDecider.Capture(w, Open)).ThenNothing();

    [Fact]
    public void Resolving_captures_a_snapshot() =>
        Decider.Given<WorkItem>(Imported, First)
            .When(w => WorkItemDecider.Capture(w, Open with { Resolved = true, Status = "Done", Hash = "h1" }))
            .Then(new WorkItemEvents.SnapshotCaptured(Open with { Resolved = true, Status = "Done", Hash = "h1" }, SnapshotReason.Resolved));

    [Fact]
    public void A_low_confidence_link_is_never_a_link() =>
        Decider.Given<WorkItem>(Imported, First)
            .When(w => WorkItemDecider.LinkSession(w, "codex:1", LinkSource.SameBranch, Confidence.Low, Open))
            .ThenThrows<DomainException>();

    [Fact]
    public void The_first_link_marks_work_started() =>
        Decider.Given<WorkItem>(Imported, First)
            .When(w => WorkItemDecider.LinkSession(w, "codex:1", LinkSource.Branch, Confidence.High, Open))
            .Then(new WorkItemEvents.SessionLinked("codex:1", LinkSource.Branch, Confidence.High), new WorkItemEvents.SnapshotCaptured(Open, SnapshotReason.WorkStarted));

    [Fact]
    public void A_stronger_link_replaces_a_weaker_one_and_a_repeat_appends_nothing()
    {
        var linked = new WorkItemEvents.SessionLinked("codex:1", LinkSource.Branch, Confidence.High);
        Decider.Given<WorkItem>(Imported, First, linked)
            .When(w => WorkItemDecider.LinkSession(w, "codex:1", LinkSource.Explicit, Confidence.Explicit, Open))
            .Then(new WorkItemEvents.SessionLinked("codex:1", LinkSource.Explicit, Confidence.Explicit));
        Decider.Given<WorkItem>(Imported, First, linked)
            .When(w => WorkItemDecider.LinkSession(w, "codex:1", LinkSource.Branch, Confidence.High, Open))
            .ThenNothing();
    }

    [Fact]
    public void A_merge_needs_a_linked_pull_request() =>
        Decider.Given<WorkItem>(Imported, First)
            .When(w => WorkItemDecider.Merge(w, new WorkItemEvents.Merged("github.com/acme/app", 7, "abc", At)))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_revert_needs_the_merge_first() =>
        Decider.Given<WorkItem>(Imported, First, Pr)
            .When(w => WorkItemDecider.ObserveRevert(w, new WorkItemEvents.RevertObserved("github.com/acme/app", 7, "github.com/acme/app#9", At)))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_fix_after_the_merge_is_observed_once()
    {
        var merged = new WorkItemEvents.Merged("github.com/acme/app", 7, "abc", At);
        var fix = new WorkItemEvents.FixObserved("github.com/acme/app", 7, "github.com/acme/app#11", 4, LinkSource.PullRequest, At.AddDays(3));
        Decider.Given<WorkItem>(Imported, First, Pr, merged).When(w => WorkItemDecider.ObserveFix(w, fix)).Then(fix);
        Decider.Given<WorkItem>(Imported, First, Pr, merged, fix).When(w => WorkItemDecider.ObserveFix(w, fix)).ThenNothing();
    }
}
