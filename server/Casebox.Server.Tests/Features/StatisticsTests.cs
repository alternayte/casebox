using System.Globalization;
using Casebox.Server.Features.Evaluations;
using static Casebox.Server.Features.Evaluations.Statistics;

namespace Casebox.Server.Tests.Features;

public sealed class StatisticsTests(ITestOutputHelper output)
{
    // The simulations use 2 000 bootstrap resamples; production uses 10 000. More resamples only
    // reduce the Monte Carlo noise of the percentile endpoints, which is symmetric and far smaller
    // than the case-sampling spread the interval measures. Run at 10 000, the no-effect rates at
    // 3, 5 and 10 repeats move by at most 0.002 from the rates at 2 000.
    private const int SimulationResamples = 2_000;
    private const double Delta = 0.05;

    // 10 to 200 in steps of 10, then to 400 in steps of 20.
    private static readonly int[] CaseCounts =
    [
        .. Enumerable.Range(1, 20).Select(i => i * 10),
        .. Enumerable.Range(1, 10).Select(i => 200 + i * 20),
    ];

    // Under no effect the whole sequential procedure (interim looks at 99.9%, the final look at
    // what remains of 5%) says better or worse at most 5% of the time, within 3 standard errors
    // of the simulation.
    [Theory]
    [InlineData(3, 1_000)]
    [InlineData(5, 1_000)]
    [InlineData(10, 1_000)]
    public void No_effect_gives_better_or_worse_at_most_5_percent_of_the_time(
        int repeats,
        int evaluations
    )
    {
        var verdicts = Simulations(evaluations, 30, repeats, 0, 0xA11CE + (ulong)repeats);
        var falsePositives = verdicts.Count(v => v is Verdict.Better or Verdict.Worse);
        var rate = (double)falsePositives / evaluations;
        var bound = 0.05 + 3 * Math.Sqrt(0.05 * 0.95 / evaluations);

        output.WriteLine(
            string.Create(
                CultureInfo.InvariantCulture,
                $"no effect, 30 cases x {repeats}: better or worse in {falsePositives}/{evaluations} = {rate:F3} (bound {bound:F4})"
            )
        );
        Assert.True(rate <= bound, $"false positive rate {rate} exceeds {bound}");
    }

    // The table behind every detectable effect Casebox prints: per repeat count, the smallest
    // case count with power 0.8 for a 10-point effect.
    [Theory]
    [InlineData(1)]
    [InlineData(2)]
    [InlineData(3)]
    [InlineData(4)]
    [InlineData(5)]
    [InlineData(6)]
    [InlineData(7)]
    [InlineData(8)]
    [InlineData(9)]
    [InlineData(10)]
    public void A_10_point_effect_is_detected_at_the_tabled_case_count(int repeats)
    {
        var found = SmallestCasesWithPower(repeats);

        Assert.NotNull(found);
        Assert.Equal(CasesFor10PointEffect[repeats], found);
    }

    // The square-root scaling holds at the small sizes people run: the effect Casebox prints for
    // 30 cases × 3 repeats and for the 10-case smoke suite × 1 repeat is detected with power 0.8,
    // within 3 standard errors of the simulation.
    [Theory]
    [InlineData(30, 3)]
    [InlineData(10, 1)]
    public void The_printed_detectable_effect_has_power_0_8(int cases, int repeats)
    {
        var effect = DetectableEffect(cases, repeats)!.Value;
        var power = Power(cases, repeats, effect);
        output.WriteLine(
            string.Create(
                CultureInfo.InvariantCulture,
                $"{cases} cases x {repeats}: detectable effect {effect:F3}, power {power:F3}"
            )
        );
        Assert.True(power >= 0.8 - 3 * Math.Sqrt(0.8 * 0.2 / 500), $"power {power}");
    }

    // "No difference detected": inconclusive, straddling 0, and within ±2δ.
    [Theory]
    [InlineData(Verdict.Inconclusive, -0.06, 0.04, 30, NoDifference)]
    [InlineData(Verdict.Inconclusive, -0.02, 0.0999, 30, NoDifference)]
    [InlineData(Verdict.Inconclusive, -0.12, 0.04, 30, null)]
    [InlineData(Verdict.Inconclusive, -0.02, 0.10, 30, null)]
    [InlineData(Verdict.Inconclusive, -0.06, 0.04, 9, null)]
    [InlineData(Verdict.Equivalent, -0.04, 0.04, 30, null)]
    [InlineData(Verdict.Better, 0.01, 0.08, 30, null)]
    public void No_difference_is_a_narrow_inconclusive_interval_around_0(
        Verdict verdict,
        double lower,
        double upper,
        int cases,
        string? reason
    ) => Assert.Equal(reason, InconclusiveReason(verdict, lower, upper, Delta, cases));

    // The t-widening cannot push a pass-rate difference past ±100 points.
    [Fact]
    public void The_interval_stays_within_the_possible_difference()
    {
        var cases = Enumerable
            .Range(0, 10)
            .Select(i => Case($"c{i}", 1, [i >= 7], [true]))
            .ToList();
        var c = Evaluate(cases, 0.95, Delta, 2_000, 7);
        Assert.InRange(c.Upper, c.Delta, 1);
        Assert.InRange(c.Lower, -1, c.Delta);
    }

    [Fact]
    public void Equivalence_under_no_effect_is_reported_at_30_60_and_120_cases()
    {
        const int evaluations = 500;
        var rates = new List<double>();
        foreach (var cases in new[] { 30, 60, 120 })
        {
            var verdicts = Simulations(evaluations, cases, 3, 0, 0xE0 + (ulong)cases);
            var rate = (double)verdicts.Count(v => v == Verdict.Equivalent) / evaluations;
            rates.Add(rate);
            output.WriteLine(
                string.Create(
                    CultureInfo.InvariantCulture,
                    $"no effect, delta {Delta}, {cases} cases x 3: equivalent {rate:F3}"
                )
            );
        }
        Assert.Equal(rates.Order(), rates);
    }

    [Fact]
    public void The_level_spends_the_error_over_the_looks()
    {
        Assert.Equal(0.95, Level(1, 1), 12);
        Assert.Equal(0.999, Level(1, 3), 12);
        Assert.Equal(0.999, Level(2, 3), 12);
        Assert.Equal(0.952, Level(3, 3), 12);
        Assert.Equal(0.959, Level(10, 10), 12);
        Assert.Equal(0.975, Level(40, 40), 12);
        Assert.Throws<ArgumentOutOfRangeException>(() => Level(4, 3));
    }

    [Theory]
    [InlineData(0.975, 1.959963984540054)]
    [InlineData(0.9995, 3.290526731491926)]
    [InlineData(0.01, -2.326347874040841)]
    public void The_normal_quantile_matches_known_values(double p, double z) =>
        Assert.Equal(z, NormalQuantile(p), 9);

    [Theory]
    [InlineData(0.975, 1, 12.70620473617471)]
    [InlineData(0.975, 9, 2.262157162798205)]
    [InlineData(0.975, 29, 2.045229642132703)]
    [InlineData(0.9995, 29, 3.659405019466)]
    [InlineData(0.025, 29, -2.045229642132703)]
    public void The_t_quantile_matches_known_values(double p, int degrees, double t) =>
        Assert.Equal(t, StudentQuantile(p, degrees), 8);

    [Theory]
    [InlineData(0.0, 0.1, 10, Verdict.Inconclusive)]
    [InlineData(1e-9, 0.1, 10, Verdict.Better)]
    [InlineData(-0.1, 0.0, 10, Verdict.Inconclusive)]
    [InlineData(-0.1, -1e-9, 10, Verdict.Worse)]
    [InlineData(-0.05, 0.05, 10, Verdict.Equivalent)]
    [InlineData(-0.0500001, 0.05, 10, Verdict.Inconclusive)]
    [InlineData(-0.05, 0.0500001, 10, Verdict.Inconclusive)]
    [InlineData(0.01, 0.04, 10, Verdict.Better)]
    [InlineData(0.2, 0.4, 9, Verdict.Inconclusive)]
    [InlineData(-0.01, 0.01, 9, Verdict.Inconclusive)]
    public void The_verdict_follows_the_interval(
        double lower,
        double upper,
        int cases,
        Verdict expected
    ) => Assert.Equal(expected, Decide(lower, upper, Delta, cases));

    [Fact]
    public void A_clear_improvement_is_better_from_10_cases()
    {
        var cases = Cases(10, baseline: [false, false], candidate: [true, true]);
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(10, checkpoint.Cases);
        Assert.Equal(1, checkpoint.Delta);
        Assert.Equal(1, checkpoint.Lower);
        Assert.Equal(1, checkpoint.Upper);
        Assert.Equal(Verdict.Better, checkpoint.Verdict);
        Assert.Equal(0, checkpoint.BaselineRate);
        Assert.Equal(1, checkpoint.CandidateRate);
    }

    [Fact]
    public void Fewer_than_10_cases_are_inconclusive()
    {
        var cases = Cases(9, baseline: [false], candidate: [true]);
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(9, checkpoint.Cases);
        Assert.Equal(1, checkpoint.Lower);
        Assert.Equal(Verdict.Inconclusive, checkpoint.Verdict);
    }

    [Fact]
    public void Only_cases_with_a_run_on_both_sides_count()
    {
        var cases = Cases(10, baseline: [false], candidate: [true])
            .Append(Case("no-candidate", 1, [true], []))
            .Append(Case("no-baseline", 1, [], [false]))
            .ToList();
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(10, checkpoint.Cases);
        Assert.Equal(1, checkpoint.Delta);
        Assert.Equal(Verdict.Better, checkpoint.Verdict);
    }

    [Fact]
    public void Weights_scale_each_case()
    {
        var cases = new List<CaseRuns>
        {
            Case("a", 1, [false], [true]),
            Case("b", 0.5, [true], [true]),
        };
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(2.0 / 3.0, checkpoint.Delta, 12);
    }

    [Fact]
    public void Equal_rates_and_half_the_cost_are_equivalent_and_cheaper()
    {
        var cases = Enumerable
            .Range(0, 12)
            .Select(i =>
                Case($"c{i}", 1, [true, false], [false, true]) with
                {
                    BaselineCost = [1.0 + i, 3.0 + i],
                    CandidateCost = [1.0 + i, 1.0 + i],
                    BaselineSeconds = [60, 60],
                    CandidateSeconds = [120],
                }
            )
            .Append(
                Case("free-baseline", 1, [true], [true]) with
                {
                    BaselineCost = [0.0],
                    CandidateCost = [5.0],
                }
            )
            .ToList();
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(13, checkpoint.Cases);
        Assert.Equal(Verdict.Equivalent, checkpoint.Verdict);
        var expected = Math.Exp(
            Enumerable.Range(0, 12).Average(i => Math.Log((1.0 + i) / (2.0 + i)))
        );
        Assert.Equal(expected, checkpoint.CostRatio!.Value, 12);
        Assert.True(checkpoint.CostUpper < 1);
        Assert.True(checkpoint.CostLower <= checkpoint.CostRatio);
        Assert.True(checkpoint.EquivalentAndCheaper);
        Assert.Equal(2, checkpoint.DurationRatio!.Value, 12);
        Assert.Equal(2, checkpoint.DurationLower!.Value, 12);
        Assert.Equal(2, checkpoint.DurationUpper!.Value, 12);
    }

    [Fact]
    public void Equivalent_at_the_same_cost_is_not_cheaper()
    {
        var cases = Enumerable
            .Range(0, 12)
            .Select(i =>
                Case($"c{i}", 1, [true], [true]) with
                {
                    BaselineCost = [2.0],
                    CandidateCost = [2.0],
                }
            )
            .ToList();
        var checkpoint = Evaluate(cases, 0.95, Delta, 1_000, 1);

        Assert.Equal(Verdict.Equivalent, checkpoint.Verdict);
        Assert.Equal(1, checkpoint.CostRatio);
        Assert.False(checkpoint.EquivalentAndCheaper);
        Assert.Null(checkpoint.DurationRatio);
    }

    [Fact]
    public void The_same_seed_gives_the_same_numbers()
    {
        var rng = new Rng(7);
        var cases = Enumerable
            .Range(0, 30)
            .Select(i =>
                Case(
                    $"c{i}",
                    i % 4 == 0 ? 0.5 : 1,
                    [.. Enumerable.Range(0, 3).Select(_ => rng.NextDouble() < 0.5)],
                    [.. Enumerable.Range(0, 3).Select(_ => rng.NextDouble() < 0.6)]
                ) with
                {
                    BaselineCost = [1 + rng.NextDouble()],
                    CandidateCost = [1 + rng.NextDouble()],
                }
            )
            .ToList();
        var seed = SeedOf("01JEVALUATION0000000000000");

        var first = Evaluate(cases, 0.95, Delta, 10_000, seed);
        var second = Evaluate(cases, 0.95, Delta, 10_000, seed);
        var other = Evaluate(cases, 0.95, Delta, 10_000, seed + 1);

        Assert.Equal(first.Lower, second.Lower);
        Assert.Equal(first.Upper, second.Upper);
        Assert.Equal(first.CostLower, second.CostLower);
        Assert.Equal(first.CostUpper, second.CostUpper);
        Assert.NotEqual((first.Lower, first.Upper), (other.Lower, other.Upper));
        Assert.Equal(SeedOf("01JEVALUATION0000000000000"), seed);
        Assert.NotEqual(SeedOf("01JEVALUATION0000000000001"), seed);
    }

    [Fact]
    public void The_generator_is_pinned()
    {
        // xoshiro256** from SplitMix64(0): a change here would change every stored verdict's rebuild.
        var rng = new Rng(0);
        var first = rng.NextUInt64();
        Assert.Equal(new Rng(0).NextUInt64(), first);
        Assert.Equal(0x99EC_5F36_CB75_F2B4UL, first);
    }

    [Theory]
    [InlineData(0, 10, 0.0, 0.2775)]
    [InlineData(5, 10, 0.2366, 0.7634)]
    [InlineData(10, 10, 0.7225, 1.0)]
    [InlineData(81, 263, 0.2553, 0.3662)]
    public void Wilson_matches_known_intervals(int successes, int n, double lower, double upper)
    {
        var interval = Wilson(successes, n, 1.96);
        Assert.Equal(lower, interval[0], 4);
        Assert.Equal(upper, interval[1], 4);
    }

    [Fact]
    public void Sequential_stops_at_the_first_verdict()
    {
        var clear = Cases(20, baseline: [false], candidate: [true]);
        var stop = Sequential(_ => clear, 3, Delta, 1_000, 1, out var round);
        Assert.Equal(1, round);
        Assert.Equal(0.999, stop.Level);
        Assert.Equal(Verdict.Better, stop.Verdict);

        var mixed = Enumerable
            .Range(0, 20)
            .Select(i => Case($"c{i}", 1, [i % 2 == 0], [i % 3 == 0]))
            .ToList();
        var end = Sequential(_ => mixed, 3, Delta, 1_000, 1, out round);
        Assert.Equal(3, round);
        Assert.Equal(0.952, end.Level, 12);
        Assert.Equal(Verdict.Inconclusive, end.Verdict);
    }

    // The smallest case count in CaseCounts at which the sequential procedure says better for a
    // 10-point effect in at least 80% of 500 simulated evaluations.
    private int? SmallestCasesWithPower(int repeats)
    {
        foreach (var cases in CaseCounts)
        {
            var power = Power(cases, repeats);
            output.WriteLine(
                string.Create(
                    CultureInfo.InvariantCulture,
                    $"10-point effect, {cases} cases x {repeats}: power {power:F3}"
                )
            );
            if (power >= 0.8)
            {
                output.WriteLine(
                    $"smallest case count with power >= 0.8 at {repeats} repeats: {cases}"
                );
                return cases;
            }
        }
        return null;
    }

    private static double Power(int cases, int repeats, double effect = 0.10)
    {
        const int evaluations = 500;
        var verdicts = Simulations(
            evaluations,
            cases,
            repeats,
            effect,
            0xB0B + (ulong)(cases * 100 + repeats)
        );
        return (double)verdicts.Count(v => v == Verdict.Better) / evaluations;
    }

    // Runs `count` simulated evaluations through the sequential procedure: per case a base rate
    // uniform in [0.2, 0.8], the candidate at base + effect (clipped to 1), `repeats` rounds.
    // Each simulation has its own seed, so the result does not depend on thread scheduling.
    private static Verdict[] Simulations(
        int count,
        int cases,
        int repeats,
        double effect,
        ulong seed
    )
    {
        var verdicts = new Verdict[count];
        Parallel.For(
            0,
            count,
            new ParallelOptions { CancellationToken = TestContext.Current.CancellationToken },
            i =>
            {
                var rng = new Rng(seed * 1_000_003 + (ulong)i);
                var baseline = new bool[cases][];
                var candidate = new bool[cases][];
                for (var c = 0; c < cases; c++)
                {
                    var rate = 0.2 + 0.6 * rng.NextDouble();
                    var candidateRate = Math.Min(1, rate + effect);
                    baseline[c] =
                    [
                        .. Enumerable.Range(0, repeats).Select(_ => rng.NextDouble() < rate),
                    ];
                    candidate[c] =
                    [
                        .. Enumerable
                            .Range(0, repeats)
                            .Select(_ => rng.NextDouble() < candidateRate),
                    ];
                }
                IReadOnlyList<CaseRuns> Round(int round) =>
                    [
                        .. Enumerable
                            .Range(0, cases)
                            .Select(c =>
                                Case($"c{c}", 1, baseline[c][..round], candidate[c][..round])
                            ),
                    ];
                verdicts[i] = Sequential(
                    Round,
                    repeats,
                    Delta,
                    SimulationResamples,
                    rng.NextUInt64(),
                    out _
                ).Verdict;
            }
        );
        return verdicts;
    }

    private static List<CaseRuns> Cases(int count, bool[] baseline, bool[] candidate) =>
        [.. Enumerable.Range(0, count).Select(i => Case($"c{i}", 1, baseline, candidate))];

    private static CaseRuns Case(string id, double weight, bool[] baseline, bool[] candidate) =>
        new(id, weight, baseline, candidate, [], [], [], []);
}
