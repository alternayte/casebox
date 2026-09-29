namespace Casebox.Server.Features.Steering;

// One stored event of a session, as the in-session detector reads it.
public sealed record TraceEvent(
    long Seq,
    DateTimeOffset At,
    string Kind,
    string? Text,
    string? Denial
);

// An intervention inside a session: its signal, when it began, the human's words and the
// sequence numbers of its human events.
public sealed record SessionIntervention(
    string Id,
    Signal Signal,
    DateTimeOffset At,
    string? Text,
    IReadOnlyList<long> Seqs
);

// docs/specs/steering.md, "In-session grouping". A human event after agent activity opens an
// intervention; human events before the next agent event join it. The task (human events before
// any agent activity) is not an intervention.
public static class InSession
{
    public const long OtelSeqBase = 2_000_000_000;
    public const long EntireSeqBase = 3_000_000_000;

    // Rewind is the strongest signal, a plain follow-up the weakest.
    private static readonly Signal[] Strength =
    [
        Signal.Rewind,
        Signal.Interruption,
        Signal.HumanEdit,
        Signal.Denial,
        Signal.FollowUp,
    ];

    // Denials a rule or an automatic review made; no person intervened.
    private static readonly HashSet<string> AutomaticDenials =
    [
        "permission-rule",
        "config",
        "auto-review",
    ];

    public static bool IsOtel(long seq) => seq is >= OtelSeqBase and < EntireSeqBase;

    // Events must come ordered by time, then sequence number.
    public static IReadOnlyList<SessionIntervention> Detect(IReadOnlyList<TraceEvent> events)
    {
        // Transcripts are the source for the conversation; OTel prompts and denials count only for
        // a session with no transcript conversation (docs/specs/capture.md).
        var hasTranscript = events.Any(e => !IsOtel(e.Seq) && e.Kind is "prompt" or "response");
        var source = hasTranscript ? events.Where(e => !IsOtel(e.Seq)) : events;

        var result = new List<SessionIntervention>();
        var agentActive = false;
        List<(TraceEvent Event, Signal Signal)>? open = null;

        void Close()
        {
            if (open is null)
                return;
            var signal = Strength.First(s => open.Any(o => o.Signal == s));
            var prompts = open.Where(o =>
                    o.Event.Kind == "prompt" && !string.IsNullOrWhiteSpace(o.Event.Text)
                )
                .Select(o => o.Event.Text!)
                .ToList();
            var text = prompts.Count == 0 ? null : string.Join("\n\n", prompts);
            result.Add(
                new SessionIntervention(
                    $"e:{open[0].Event.Seq}",
                    signal,
                    open[0].Event.At,
                    text,
                    open.Select(o => o.Event.Seq).ToList()
                )
            );
            open = null;
        }

        foreach (var e in source)
        {
            if (e.Kind is "response" or "tool_call" or "tool_result")
            {
                Close();
                agentActive = true;
                continue;
            }

            if (HumanSignal(e) is not { } signal)
                continue;
            if (!agentActive)
                continue; // the task, or an amendment to it before the agent acts
            open ??= [];
            open.Add((e, signal));
        }

        Close();
        return result;
    }

    private static Signal? HumanSignal(TraceEvent e) =>
        e.Kind switch
        {
            "prompt" when !IsCommand(e.Text) => Signal.FollowUp,
            "interruption" => Signal.Interruption,
            "denial" when e.Denial is null || !AutomaticDenials.Contains(e.Denial) => Signal.Denial,
            "rewind" => Signal.Rewind,
            "human_edit" => Signal.HumanEdit,
            _ => null,
        };

    // A slash command or its local output is the agent's interface, not an instruction.
    private static bool IsCommand(string? text)
    {
        var t = text?.TrimStart();
        return t is not null
            && (
                t.StartsWith("<command-", StringComparison.Ordinal)
                || t.StartsWith("<local-command-", StringComparison.Ordinal)
            );
    }
}
