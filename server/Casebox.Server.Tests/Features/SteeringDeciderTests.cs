using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Deedbox.Testing;

namespace Casebox.Server.Tests.Features;

public sealed class SteeringDeciderTests
{
    private static readonly DateTimeOffset At = new(2026, 9, 1, 10, 0, 0, TimeSpan.Zero);

    private static SteeringEvents.Observed Observed(
        string id = "e:5",
        string? text = "no, keep the old API",
        Intent? rule = null
    ) =>
        new(
            id,
            Signal.FollowUp,
            Phase.InSession,
            At,
            "github.com/acme/app",
            "claude-code:s1",
            null,
            "person:aaaaaaaaaaaaaaaaaaaaaaaaaa",
            true,
            "2026-Q3",
            text,
            rule,
            new SteeringRefs(Seqs: [5])
        );

    [Fact]
    public void An_intervention_is_observed_once() =>
        Decider
            .Given<SteeringState>(Observed())
            .When(s => SteeringDecider.Observe(s, [Observed(), Observed("e:9")]))
            .Then(Observed("e:9"));

    [Fact]
    public void An_intervention_without_text_or_rule_intent_is_unclassified_at_once() =>
        Decider
            .Given<SteeringState>()
            .When(s => SteeringDecider.Observe(s, [Observed(text: " ")]))
            .Then(
                Observed(text: null),
                new SteeringEvents.Unclassified("e:5", UnclassifiedReason.NoText, null, null, null)
            );

    [Fact]
    public void A_rule_correction_without_text_waits_for_its_classification() =>
        Decider
            .Given<SteeringState>()
            .When(s => SteeringDecider.Observe(s, [Observed(text: null, rule: Intent.Correction)]))
            .Then(Observed(text: null, rule: Intent.Correction));

    [Fact]
    public void A_rule_intent_wins_over_the_model() =>
        Decider
            .Given<SteeringState>(Observed(rule: Intent.Correction))
            .When(s =>
                SteeringDecider.Classify(
                    s,
                    new SteeringEvents.Classified(
                        "e:5",
                        Intent.Direction,
                        WentWrong.WrongApproach,
                        null,
                        Prevention.Skill,
                        0.8,
                        "m",
                        "v1"
                    )
                )
            )
            .Then(
                new SteeringEvents.Classified(
                    "e:5",
                    Intent.Correction,
                    WentWrong.WrongApproach,
                    null,
                    Prevention.Skill,
                    0.8,
                    "m",
                    "v1"
                )
            );

    [Fact]
    public void A_classification_is_recorded_once_per_model_and_prompt_version() =>
        Decider
            .Given<SteeringState>(
                Observed(),
                new SteeringEvents.Classified(
                    "e:5",
                    Intent.Direction,
                    null,
                    null,
                    null,
                    0.9,
                    "m",
                    "v1"
                )
            )
            .When(s =>
                SteeringDecider.Classify(
                    s,
                    new SteeringEvents.Classified(
                        "e:5",
                        Intent.Direction,
                        null,
                        null,
                        null,
                        0.9,
                        "m",
                        "v1"
                    )
                )
            )
            .ThenNothing();

    [Fact]
    public void Only_an_observed_intervention_is_classified() =>
        Decider
            .Given<SteeringState>()
            .When(s =>
                SteeringDecider.Classify(
                    s,
                    new SteeringEvents.Classified(
                        "e:5",
                        Intent.Direction,
                        null,
                        null,
                        null,
                        0.9,
                        "m",
                        "v1"
                    )
                )
            )
            .ThenThrows<NotFoundException>();

    [Fact]
    public void A_correction_needs_what_went_wrong_and_its_prevention() =>
        Decider
            .Given<SteeringState>(Observed())
            .When(s =>
                SteeringDecider.Relabel(
                    s,
                    "e:5",
                    new Labels(Intent.Correction, WentWrong.BrokeConvention, null, null),
                    "acct"
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void Other_needs_a_short_label() =>
        Decider
            .Given<SteeringState>(Observed())
            .When(s =>
                SteeringDecider.Relabel(
                    s,
                    "e:5",
                    new Labels(
                        Intent.Correction,
                        WentWrong.Other,
                        "used the wrong logging library for this service ever",
                        Prevention.Instruction
                    ),
                    "acct"
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void A_relabel_carries_the_person_so_erasure_reaches_it() =>
        Decider
            .Given<SteeringState>(Observed())
            .When(s =>
                SteeringDecider.Relabel(
                    s,
                    "e:5",
                    new Labels(
                        Intent.Correction,
                        WentWrong.BrokeConvention,
                        null,
                        Prevention.Instruction
                    ),
                    "acct"
                )
            )
            .Then(
                new SteeringEvents.Relabeled(
                    "e:5",
                    "person:aaaaaaaaaaaaaaaaaaaaaaaaaa",
                    Intent.Correction,
                    WentWrong.BrokeConvention,
                    null,
                    Prevention.Instruction,
                    "acct"
                )
            );

    [Fact]
    public void The_same_relabel_twice_appends_nothing() =>
        Decider
            .Given<SteeringState>(
                Observed(),
                new SteeringEvents.Relabeled(
                    "e:5",
                    "person:aaaaaaaaaaaaaaaaaaaaaaaaaa",
                    Intent.Direction,
                    null,
                    null,
                    null,
                    "acct"
                )
            )
            .When(s =>
                SteeringDecider.Relabel(
                    s,
                    "e:5",
                    new Labels(Intent.Direction, null, null, null),
                    "other"
                )
            )
            .ThenNothing();
}

public sealed class InSessionTests
{
    private static readonly DateTimeOffset T0 = new(2026, 9, 1, 10, 0, 0, TimeSpan.Zero);

    private static TraceEvent E(
        long seq,
        string kind,
        string? text = null,
        string? denial = null
    ) => new(seq, T0.AddSeconds(seq), kind, text, denial);

    [Fact]
    public void The_task_is_not_an_intervention_and_a_follow_up_after_agent_work_is()
    {
        var found = InSession.Detect([
            E(0, "prompt", "add retries"),
            E(1, "prompt", "and log them"),
            E(2, "response", "done"),
            E(3, "prompt", "no, keep the old API"),
        ]);
        var i = Assert.Single(found);
        Assert.Equal(
            ("e:3", Signal.FollowUp, T0.AddSeconds(3), "no, keep the old API"),
            (i.Id, i.Signal, i.At, i.Text)
        );
        Assert.Equal([3], i.Seqs);
    }

    [Fact]
    public void Human_events_between_two_agent_turns_are_one_intervention_with_the_strongest_signal()
    {
        var found = InSession.Detect([
            E(0, "prompt", "task"),
            E(1, "tool_call"),
            E(2, "interruption"),
            E(3, "rewind"),
            E(4, "prompt", "not that file"),
            E(5, "response"),
        ]);
        var i = Assert.Single(found);
        Assert.Equal(("e:2", Signal.Rewind, "not that file"), (i.Id, i.Signal, i.Text));
        Assert.Equal([2, 3, 4], i.Seqs);
    }

    [Fact]
    public void Automatic_denials_and_slash_commands_are_not_a_person_intervening()
    {
        var found = InSession.Detect([
            E(0, "prompt", "task"),
            E(1, "tool_call"),
            E(2, "denial", denial: "permission-rule"),
            E(3, "tool_call"),
            E(4, "denial", denial: "auto-review"),
            E(5, "response"),
            E(6, "prompt", "<command-name>/clear</command-name>"),
            E(7, "response"),
            E(8, "denial", denial: "user-rejected"),
            E(9, "tool_call"),
        ]);
        var i = Assert.Single(found);
        Assert.Equal(("e:8", Signal.Denial, (string?)null), (i.Id, i.Signal, i.Text));
    }

    [Fact]
    public void OTel_prompts_count_only_when_no_transcript_holds_the_conversation()
    {
        var otel = InSession.OtelSeqBase;
        TraceEvent[] withTranscript =
        [
            E(0, "prompt", "task"),
            E(1, "response"),
            E(otel + 1, "prompt", "from otel"),
            E(2, "prompt", "from transcript"),
        ];
        Assert.Equal(
            ["from transcript"],
            InSession.Detect(withTranscript.OrderBy(e => e.At).ToList()).Select(i => i.Text)
        );

        TraceEvent[] otelOnly =
        [
            E(otel, "prompt", "task"),
            E(otel + 1, "tool_result"),
            E(otel + 2, "prompt", "stop, wrong branch"),
        ];
        Assert.Equal(["stop, wrong branch"], InSession.Detect(otelOnly).Select(i => i.Text));
    }

    [Fact]
    public void A_human_edit_joins_the_prompt_that_follows_it()
    {
        var found = InSession.Detect([
            E(0, "prompt", "task"),
            E(1, "response"),
            E(2, "human_edit"),
            E(3, "prompt", "I fixed the import, go on"),
            E(4, "tool_call"),
        ]);
        var i = Assert.Single(found);
        Assert.Equal((Signal.HumanEdit, "I fixed the import, go on"), (i.Signal, i.Text));
    }
}

public sealed class SteeringMathTests
{
    [Fact]
    public void Data_across_two_periods_never_counts_one_person_twice()
    {
        // One person has a different token in each quarter; two people in the busiest period.
        Person[] people =
        [
            new("q3-ada", true, "2026-Q3"),
            new("q3-bob", true, "2026-Q3"),
            new("q4-ada", true, "2026-Q4"),
            new("q4-bob", true, "2026-Q4"),
            new("q4-x", false, "2026-Q4"),
        ];
        Assert.Equal(2, KRule.People(people));
        Assert.False(KRule.Meets(people, 3));
    }

    [Fact]
    public void The_Wilson_interval_matches_the_textbook_value() =>
        // 8 of 10: the 95% Wilson interval is 0.490 to 0.943.
        Assert.Equal([0.4902, 0.9433], SteeringReports.Wilson(8, 10));

    [Fact]
    public void Kappa_discounts_the_agreement_two_raters_reach_by_chance()
    {
        // 50 pairs: 20 yes-yes, 15 no-no, 5 yes-no, 10 no-yes. Observed 0.70, chance 0.50, kappa 0.40.
        var pairs = Enumerable
            .Repeat(("yes", "yes"), 20)
            .Concat(Enumerable.Repeat(("no", "no"), 15))
            .Concat(Enumerable.Repeat(("yes", "no"), 5))
            .Concat(Enumerable.Repeat(("no", "yes"), 10))
            .ToList();
        Assert.Equal(
            new SteeringEndpoints.StepAgreement(0.7, 0.4, 50),
            SteeringEndpoints.Agreement(pairs)
        );
    }

    [Fact]
    public void Task_type_comes_from_labels_before_the_issue_type() =>
        Assert.Equal(
            new string?[] { "refactor", "bug", "feature", "docs", null },
            new[]
            {
                TaskTypes.Of("Bug", """["Refactor"]"""),
                TaskTypes.Of("Bug", "[]"),
                TaskTypes.Of("issue", """["enhancement"]"""),
                TaskTypes.Of("Story", """["documentation"]"""),
                TaskTypes.Of("Task", null),
            }
        );
}
