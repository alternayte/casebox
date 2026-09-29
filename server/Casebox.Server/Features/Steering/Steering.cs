using System.Collections.Immutable;
using Deedbox;

namespace Casebox.Server.Features.Steering;

public enum Signal
{
    FollowUp,
    Interruption,
    Denial,
    Rewind,
    HumanEdit,
    Abandoned,
    Restarted,
    HumanRewrite,
    ReviewChange,
    CiFix,
    Revert,
    Fix,
}

public enum Phase
{
    InSession,
    BeforeMerge,
    AfterMerge,
}

public enum Intent
{
    Correction,
    Direction,
    Clarification,
    Routine,
}

public enum WentWrong
{
    MissedRequirement,
    BrokeConvention,
    WrongApproach,
    UnverifiedDone,
    WrongArea,
    OverEngineered,
    LackedDomainKnowledge,
    Environment,
    Other,
}

public enum Prevention
{
    Instruction,
    Skill,
    ToolAccess,
    Verification,
    ClearerTicket,
    StrongerModel,
    Nothing,
}

public enum TaskType
{
    Bug,
    Feature,
    Refactor,
    Test,
    Config,
    Docs,
}

public enum UnclassifiedReason
{
    NoText,
    LowConfidence,
    InvalidOutput,
    ModelError,
}

// What a worker needs to rebuild an intervention's window: the human events of an in-session
// intervention, the session that restarted it, the commits whose diff and message explain it, or
// the review comment behind it.
public sealed record SteeringRefs(
    IReadOnlyList<long>? Seqs = null,
    string? RestartedBy = null,
    IReadOnlyList<string>? Commits = null,
    long? CommentId = null
);

// The labels of one intervention. What went wrong and its prevention exist only for corrections.
public sealed record Labels(
    Intent Intent,
    WentWrong? WentWrong,
    string? WentWrongLabel,
    Prevention? Prevention
)
{
    // A problem with the labels, or null when they are valid.
    public string? Problem()
    {
        if (Intent != Intent.Correction)
            return WentWrong is null && Prevention is null && WentWrongLabel is null
                ? null
                : "Only a correction has what went wrong and a prevention.";
        if (WentWrong is null || Prevention is null)
            return "A correction needs what went wrong and what would have prevented it.";
        if (WentWrong != Steering.WentWrong.Other)
            return WentWrongLabel is null ? null : "A short label goes only with 'other'.";
        if (string.IsNullOrWhiteSpace(WentWrongLabel))
            return "'Other' needs a short label.";
        return
            WentWrongLabel.Split(' ', StringSplitOptions.RemoveEmptyEntries).Length > 6
            || WentWrongLabel.Length > 60
            ? "The label is at most 6 words."
            : null;
    }
}

public static class SteeringEvents
{
    // A human intervention. Person is the subject of the human who intervened; Text is their own
    // words, encrypted under their key, so erasing them makes it unreadable in every stream.
    public sealed record Observed(
        string InterventionId,
        Signal Signal,
        Phase Phase,
        DateTimeOffset At,
        string Repo,
        string? SessionId,
        int? Number,
        [property: DataSubject] string Person,
        bool PersonMapped,
        string Period,
        [property: PersonalData] string? Text,
        Intent? RuleIntent,
        SteeringRefs Refs
    );

    public sealed record Classified(
        string InterventionId,
        Intent Intent,
        WentWrong? WentWrong,
        string? WentWrongLabel,
        Prevention? Prevention,
        double Confidence,
        string Model,
        string PromptVersion
    );

    public sealed record Unclassified(
        string InterventionId,
        UnclassifiedReason Reason,
        double? Confidence,
        string? Model,
        string? PromptVersion
    );

    // A person corrected the labels. It beats every classification, older or newer.
    public sealed record Relabeled(
        string InterventionId,
        [property: DataSubject] string Person,
        Intent Intent,
        WentWrong? WentWrong,
        string? WentWrongLabel,
        Prevention? Prevention,
        string By
    );
}

public sealed record Intervention(
    Signal Signal,
    string Person,
    bool HasText,
    Intent? RuleIntent,
    ImmutableHashSet<string> Outcomes,
    Labels? Relabel
);

// One steering stream: the interventions of a session (steering:session:<id>) or of a pull
// request (steering:pr:<repo>#<n>).
public sealed record SteeringState(ImmutableDictionary<string, Intervention> Interventions)
    : IState<SteeringState>
{
    public static SteeringState Initial { get; } =
        new(ImmutableDictionary<string, Intervention>.Empty);

    public static SteeringState Evolve(SteeringState s, object e) =>
        e switch
        {
            SteeringEvents.Observed x => s with
            {
                Interventions = s.Interventions.SetItem(
                    x.InterventionId,
                    new Intervention(
                        x.Signal,
                        x.Person,
                        x.Text is not null,
                        x.RuleIntent,
                        ImmutableHashSet<string>.Empty,
                        null
                    )
                ),
            },
            SteeringEvents.Classified x => s.With(
                x.InterventionId,
                i => i with { Outcomes = i.Outcomes.Add(ClassifiedKey(x.Model, x.PromptVersion)) }
            ),
            SteeringEvents.Unclassified x => s.With(
                x.InterventionId,
                i =>
                    i with
                    {
                        Outcomes = i.Outcomes.Add(
                            UnclassifiedKey(x.Reason, x.Model, x.PromptVersion)
                        ),
                    }
            ),
            SteeringEvents.Relabeled x => s.With(
                x.InterventionId,
                i =>
                    i with
                    {
                        Relabel = new Labels(x.Intent, x.WentWrong, x.WentWrongLabel, x.Prevention),
                    }
            ),
            _ => s,
        };

    private SteeringState With(string id, Func<Intervention, Intervention> change) =>
        Interventions.TryGetValue(id, out var i)
            ? this with
            {
                Interventions = Interventions.SetItem(id, change(i)),
            }
            : this;

    public static string ClassifiedKey(string model, string version) =>
        $"classified|{model}|{version}";

    public static string UnclassifiedKey(
        UnclassifiedReason reason,
        string? model,
        string? version
    ) => $"unclassified|{reason}|{model}|{version}";

    public static string SessionStream(string sessionId) => $"steering:session:{sessionId}";

    public static string PullRequestStream(string repo, int number) =>
        $"steering:pr:{repo}#{number}";
}

public static class SteeringDecider
{
    public const int MaxText = 4000;

    // Each intervention is observed once; later detection passes append nothing. One with no text
    // and no rule intent has nothing a model could classify: it counts in the structural numbers only.
    public static IEnumerable<object> Observe(
        SteeringState s,
        IEnumerable<SteeringEvents.Observed> observed
    )
    {
        var seen = new HashSet<string>(s.Interventions.Keys, StringComparer.Ordinal);
        foreach (var o in observed)
        {
            if (!seen.Add(o.InterventionId))
                continue;
            if (string.IsNullOrEmpty(o.Person))
                throw new DomainException("An intervention needs the person who intervened.");
            var text =
                string.IsNullOrWhiteSpace(o.Text) ? null
                : o.Text.Length > MaxText ? o.Text[..MaxText]
                : o.Text;
            yield return o with
            {
                Text = text,
            };
            if (text is null && o.RuleIntent is null)
                yield return new SteeringEvents.Unclassified(
                    o.InterventionId,
                    UnclassifiedReason.NoText,
                    null,
                    null,
                    null
                );
        }
    }

    // A classification is recorded once per model and prompt version. A rule intent wins over the
    // model's intent.
    public static IEnumerable<object> Classify(SteeringState s, SteeringEvents.Classified c)
    {
        var i = Require(s, c.InterventionId);
        var labels = new Labels(
            i.RuleIntent ?? c.Intent,
            c.WentWrong,
            c.WentWrongLabel,
            c.Prevention
        );
        if (labels.Problem() is { } problem)
            throw new DomainException(problem);
        if (i.Outcomes.Contains(SteeringState.ClassifiedKey(c.Model, c.PromptVersion)))
            return [];
        return [c with { Intent = labels.Intent }];
    }

    public static IEnumerable<object> Unclassify(SteeringState s, SteeringEvents.Unclassified u)
    {
        var i = Require(s, u.InterventionId);
        return i.Outcomes.Contains(
            SteeringState.UnclassifiedKey(u.Reason, u.Model, u.PromptVersion)
        )
            ? []
            : [u];
    }

    public static IEnumerable<object> Relabel(
        SteeringState s,
        string interventionId,
        Labels labels,
        string by
    )
    {
        var i = Require(s, interventionId);
        if (labels.Problem() is { } problem)
            throw new DomainException(problem);
        if (i.Relabel == labels)
            return [];
        return
        [
            new SteeringEvents.Relabeled(
                interventionId,
                i.Person,
                labels.Intent,
                labels.WentWrong,
                labels.WentWrongLabel,
                labels.Prevention,
                by
            ),
        ];
    }

    private static Intervention Require(SteeringState s, string id) =>
        s.Interventions.TryGetValue(id, out var i)
            ? i
            : throw new NotFoundException($"Intervention {id} has not been observed.");
}
