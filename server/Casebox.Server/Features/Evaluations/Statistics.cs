using System.Buffers.Binary;
using System.Security.Cryptography;
using System.Text;

namespace Casebox.Server.Features.Evaluations;

// The evaluation statistics: paired bootstrap over cases, the verdict rules, Wilson intervals
// and the sequential checkpoints. Pure and deterministic: the same input and seed give the
// same numbers in any process and on any .NET version.
public static class Statistics
{
    public const int Resamples = 10_000;

    // Haybittle–Peto: every interim look spends 0.001 of the two-sided error; the final look
    // gets what remains of 0.05, so the whole sequential procedure stays within 5%.
    public const double InterimLevel = 0.999;
    public const double TotalError = 0.05;
    public const double MinimumFinalError = 0.025;
    public const int MinimumCases = 10;

    // Per repeat count (index 1 to 10), the smallest case count at which the sequential
    // procedure detects a 10-point effect with power of at least 0.8, found by the simulation in
    // StatisticsTests (base rates uniform in [0.2, 0.8]).
    public static readonly IReadOnlyList<int> CasesFor10PointEffect =
    [
        0,
        340,
        170,
        120,
        90,
        70,
        70,
        50,
        50,
        40,
        40,
    ];

    private const double Z95 = 1.959963984540054;

    public enum Verdict
    {
        Better,
        Worse,
        Equivalent,
        Inconclusive,
    }

    // One case's completed runs: pass or fail, cost in USD and seconds per run, per side.
    public sealed record CaseRuns(
        string CaseId,
        double Weight,
        IReadOnlyList<bool> Baseline,
        IReadOnlyList<bool> Candidate,
        IReadOnlyList<double> BaselineCost,
        IReadOnlyList<double> CandidateCost,
        IReadOnlyList<double> BaselineSeconds,
        IReadOnlyList<double> CandidateSeconds
    );

    public sealed record Checkpoint(
        int Cases,
        double Delta,
        double Lower,
        double Upper,
        double Level,
        Verdict Verdict,
        double? CostRatio,
        double? CostLower,
        double? CostUpper,
        double? DurationRatio,
        double? DurationLower,
        double? DurationUpper,
        bool EquivalentAndCheaper,
        double BaselineRate,
        double[] BaselineInterval,
        double CandidateRate,
        double[] CandidateInterval
    );

    public static Checkpoint Evaluate(
        IReadOnlyList<CaseRuns> cases,
        double level,
        double delta,
        int resamples,
        ulong seed
    )
    {
        ArgumentNullException.ThrowIfNull(cases);
        ArgumentOutOfRangeException.ThrowIfLessThanOrEqual(level, 0);
        ArgumentOutOfRangeException.ThrowIfGreaterThanOrEqual(level, 1);
        ArgumentOutOfRangeException.ThrowIfNegative(delta);
        ArgumentOutOfRangeException.ThrowIfLessThan(resamples, 1);

        var counted = cases.Where(c => c.Baseline.Count > 0 && c.Candidate.Count > 0).ToList();
        foreach (var c in counted)
        {
            if (!double.IsFinite(c.Weight) || c.Weight <= 0)
            {
                throw new ArgumentException(
                    $"case {c.CaseId} has weight {c.Weight}; weights are positive",
                    nameof(cases)
                );
            }
        }

        var weights = counted.Select(c => c.Weight).ToArray();
        var differences = counted.Select(c => Rate(c.Candidate) - Rate(c.Baseline)).ToArray();

        // Each metric draws from its own stream, so adding cost data never moves the pass-rate interval.
        var (mean, lower, upper) = Bootstrap(
            differences,
            weights,
            level,
            resamples,
            new Rng(seed ^ 0x5041_5353_5241_5445UL)
        );

        // A pass-rate difference lies in [−1, 1]; the t-widening can reach past it with few cases.
        lower = Math.Max(-1, lower);
        upper = Math.Min(1, upper);
        var verdict = Decide(lower, upper, delta, counted.Count);
        var cost = Ratio(
            counted,
            c => c.BaselineCost,
            c => c.CandidateCost,
            level,
            resamples,
            new Rng(seed ^ 0x434F_5354_5241_5449UL)
        );
        var duration = Ratio(
            counted,
            c => c.BaselineSeconds,
            c => c.CandidateSeconds,
            level,
            resamples,
            new Rng(seed ^ 0x4455_5241_5449_4F4EUL)
        );

        var baselinePassed = counted.Sum(c => c.Baseline.Count(p => p));
        var baselineRuns = counted.Sum(c => c.Baseline.Count);
        var candidatePassed = counted.Sum(c => c.Candidate.Count(p => p));
        var candidateRuns = counted.Sum(c => c.Candidate.Count);

        return new Checkpoint(
            counted.Count,
            mean,
            lower,
            upper,
            level,
            verdict,
            cost?.Ratio,
            cost?.Lower,
            cost?.Upper,
            duration?.Ratio,
            duration?.Lower,
            duration?.Upper,
            verdict == Verdict.Equivalent && cost is { Upper: < 1 },
            baselineRuns == 0 ? 0 : (double)baselinePassed / baselineRuns,
            Wilson(baselinePassed, baselineRuns, Z95),
            candidateRuns == 0 ? 0 : (double)candidatePassed / candidateRuns,
            Wilson(candidatePassed, candidateRuns, Z95)
        );
    }

    // The verdict rules: an interval entirely above 0 is better, entirely below 0 is worse,
    // within ±delta (inclusive) is equivalent; anything else, or fewer than 10 cases, is inconclusive.
    public static Verdict Decide(double lower, double upper, double delta, int cases)
    {
        if (cases < MinimumCases)
        {
            return Verdict.Inconclusive;
        }
        if (lower > 0)
        {
            return Verdict.Better;
        }
        if (upper < 0)
        {
            return Verdict.Worse;
        }
        if (lower >= -delta && upper <= delta)
        {
            return Verdict.Equivalent;
        }
        return Verdict.Inconclusive;
    }

    // The smallest pass-rate difference an evaluation of this size detects with power 0.8, from
    // the simulation table: the interval's width shrinks with the square root of the case count,
    // so the effect detected at n cases is 10 points × √(cases for 10 points ÷ n). Null below the
    // minimum case count, where no verdict is given.
    public static double? DetectableEffect(int cases, int repeats)
    {
        ArgumentOutOfRangeException.ThrowIfLessThan(repeats, 1);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(repeats, CasesFor10PointEffect.Count - 1);
        if (cases < MinimumCases)
        {
            return null;
        }
        return 0.10 * Math.Sqrt((double)CasesFor10PointEffect[repeats] / cases);
    }

    // Why an inconclusive verdict is inconclusive, when its interval says more than "unclear":
    // "no_difference" when it straddles 0 and lies within ±2δ, so any real difference is small,
    // though not small enough to call the sides equivalent. Null for every other verdict.
    public static string? InconclusiveReason(
        Verdict verdict,
        double lower,
        double upper,
        double delta,
        int cases
    )
    {
        if (verdict != Verdict.Inconclusive || cases < MinimumCases)
        {
            return null;
        }
        return lower <= 0 && upper >= 0 && lower > -2 * delta && upper < 2 * delta
            ? NoDifference
            : null;
    }

    public const string NoDifference = "no_difference";

    // The interval level of the check after `round` of `rounds`: 99.9% at an interim look, and
    // 1 − (0.05 − 0.001 × interim looks) at the final look (never below 97.5%), so by the union
    // bound the error over the whole procedure is at most 5%.
    public static double Level(int round, int rounds)
    {
        ArgumentOutOfRangeException.ThrowIfLessThan(rounds, 1);
        ArgumentOutOfRangeException.ThrowIfLessThan(round, 1);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(round, rounds);
        if (round < rounds)
        {
            return InterimLevel;
        }
        var finalError = Math.Max(
            MinimumFinalError,
            TotalError - (1 - InterimLevel) * (rounds - 1)
        );
        return 1 - finalError;
    }

    // Runs the checkpoints of an evaluation: roundData(r) gives every completed run after round r.
    // Each check uses Level(r, rounds); it stops at the first verdict that is not inconclusive.
    public static Checkpoint Sequential(
        Func<int, IReadOnlyList<CaseRuns>> roundData,
        int rounds,
        double delta,
        int resamples,
        ulong seed,
        out int stoppedAtRound
    )
    {
        ArgumentNullException.ThrowIfNull(roundData);
        ArgumentOutOfRangeException.ThrowIfLessThan(rounds, 1);

        for (var round = 1; ; round++)
        {
            var level = Level(round, rounds);
            var checkpoint = Evaluate(roundData(round), level, delta, resamples, seed);
            if (checkpoint.Verdict != Verdict.Inconclusive || round == rounds)
            {
                stoppedAtRound = round;
                return checkpoint;
            }
        }
    }

    // The first 8 bytes of SHA-256 of the evaluation ID, little-endian.
    public static ulong SeedOf(string evaluationId)
    {
        ArgumentNullException.ThrowIfNull(evaluationId);
        var hash = SHA256.HashData(Encoding.UTF8.GetBytes(evaluationId));
        return BinaryPrimitives.ReadUInt64LittleEndian(hash);
    }

    // The Wilson score interval [lower, upper] for a rate; [0, 1] with no trials.
    public static double[] Wilson(int successes, int n, double z)
    {
        ArgumentOutOfRangeException.ThrowIfNegative(successes);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(successes, n);
        if (n == 0)
        {
            return [0, 1];
        }
        var p = (double)successes / n;
        var z2 = z * z;
        var denominator = 1 + z2 / n;
        var centre = (p + z2 / (2.0 * n)) / denominator;
        var half = z * Math.Sqrt(p * (1 - p) / n + z2 / (4.0 * n * n)) / denominator;
        return [Math.Max(0, centre - half), Math.Min(1, centre + half)];
    }

    private static double Rate(IReadOnlyList<bool> runs) => (double)runs.Count(p => p) / runs.Count;

    private static (double Ratio, double Lower, double Upper)? Ratio(
        List<CaseRuns> cases,
        Func<CaseRuns, IReadOnlyList<double>> baseline,
        Func<CaseRuns, IReadOnlyList<double>> candidate,
        double level,
        int resamples,
        Rng rng
    )
    {
        var logs = new List<double>();
        var weights = new List<double>();
        foreach (var c in cases)
        {
            var b = baseline(c);
            var k = candidate(c);
            if (b.Count == 0 || k.Count == 0)
            {
                continue;
            }
            var baselineMean = b.Average();
            var candidateMean = k.Average();
            // A zero mean has no finite log ratio; such a case carries no cost or duration signal.
            if (baselineMean <= 0 || candidateMean <= 0)
            {
                continue;
            }
            logs.Add(Math.Log(candidateMean / baselineMean));
            weights.Add(c.Weight);
        }
        if (logs.Count == 0)
        {
            return null;
        }
        var (mean, lower, upper) = Bootstrap([.. logs], [.. weights], level, resamples, rng);
        return (Math.Exp(mean), Math.Exp(lower), Math.Exp(upper));
    }

    // The weighted mean of the values and its percentile interval from a bootstrap over cases.
    // The percentile bootstrap undercovers with few cases: it treats the cases as the population
    // and ignores that the spread is itself estimated. The interval is therefore widened around
    // the mean by t(n−1) / z at the same level, the small-sample correction of a t interval.
    private static (double Mean, double Lower, double Upper) Bootstrap(
        double[] values,
        double[] weights,
        double level,
        int resamples,
        Rng rng
    )
    {
        var n = values.Length;
        if (n == 0)
        {
            return (0, 0, 0);
        }
        var mean = WeightedMean(values, weights);
        var means = new double[resamples];
        for (var r = 0; r < resamples; r++)
        {
            double sum = 0,
                total = 0;
            for (var i = 0; i < n; i++)
            {
                var j = rng.NextInt(n);
                sum += weights[j] * values[j];
                total += weights[j];
            }
            means[r] = sum / total;
        }
        Array.Sort(means);
        var tail = (1 - level) / 2;
        var lower = Quantile(means, tail);
        var upper = Quantile(means, 1 - tail);
        var widen = n < 2 ? 1 : StudentQuantile(1 - tail, n - 1) / NormalQuantile(1 - tail);
        return (mean, mean - (mean - lower) * widen, mean + (upper - mean) * widen);
    }

    private static double WeightedMean(double[] values, double[] weights)
    {
        double sum = 0,
            total = 0;
        for (var i = 0; i < values.Length; i++)
        {
            sum += weights[i] * values[i];
            total += weights[i];
        }
        return sum / total;
    }

    // Linear interpolation between order statistics (Hyndman and Fan type 7).
    private static double Quantile(double[] sorted, double p)
    {
        var h = (sorted.Length - 1) * p;
        var below = (int)Math.Floor(h);
        var above = Math.Min(below + 1, sorted.Length - 1);
        return sorted[below] + (h - below) * (sorted[above] - sorted[below]);
    }

    // The p quantile of the standard normal distribution (Acklam's rational approximation,
    // refined by one Halley step to full double precision).
    public static double NormalQuantile(double p)
    {
        ArgumentOutOfRangeException.ThrowIfLessThanOrEqual(p, 0);
        ArgumentOutOfRangeException.ThrowIfGreaterThanOrEqual(p, 1);
        double[] a =
        [
            -3.969683028665376e+01,
            2.209460984245205e+02,
            -2.759285104469687e+02,
            1.383577518672690e+02,
            -3.066479806614716e+01,
            2.506628277459239e+00,
        ];
        double[] b =
        [
            -5.447609879822406e+01,
            1.615858368580409e+02,
            -1.556989798598866e+02,
            6.680131188771972e+01,
            -1.328068155288572e+01,
        ];
        double[] c =
        [
            -7.784894002430293e-03,
            -3.223964580411365e-01,
            -2.400758277161838e+00,
            -2.549732539343734e+00,
            4.374664141464968e+00,
            2.938163982698783e+00,
        ];
        double[] d =
        [
            7.784695709041462e-03,
            3.224671290700398e-01,
            2.445134137142996e+00,
            3.754408661907416e+00,
        ];
        const double low = 0.02425;
        double x;
        if (p < low)
        {
            var q = Math.Sqrt(-2 * Math.Log(p));
            x =
                (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5])
                / ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1);
        }
        else if (p > 1 - low)
        {
            var q = Math.Sqrt(-2 * Math.Log(1 - p));
            x =
                -(((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5])
                / ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1);
        }
        else
        {
            var q = p - 0.5;
            var r = q * q;
            x =
                (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) * r + a[5])
                * q
                / (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) * r + 1);
        }
        var e = NormalCdf(x) - p;
        var u = e * Math.Sqrt(2 * Math.PI) * Math.Exp(x * x / 2);
        return x - u / (1 + x * u / 2);
    }

    // The p quantile of Student's t distribution with the given degrees of freedom, found by
    // bisection on its distribution function.
    public static double StudentQuantile(double p, int degrees)
    {
        ArgumentOutOfRangeException.ThrowIfLessThanOrEqual(p, 0);
        ArgumentOutOfRangeException.ThrowIfGreaterThanOrEqual(p, 1);
        ArgumentOutOfRangeException.ThrowIfLessThan(degrees, 1);
        if (p < 0.5)
        {
            return -StudentQuantile(1 - p, degrees);
        }
        double lo = 0,
            hi = 1;
        while (StudentCdf(hi, degrees) < p)
        {
            hi *= 2;
        }
        for (var i = 0; i < 200 && hi - lo > 1e-12 * hi; i++)
        {
            var mid = (lo + hi) / 2;
            if (StudentCdf(mid, degrees) < p)
            {
                lo = mid;
            }
            else
            {
                hi = mid;
            }
        }
        return (lo + hi) / 2;
    }

    private static double StudentCdf(double t, int degrees)
    {
        var x = degrees / (degrees + t * t);
        var tail = 0.5 * RegularizedBeta(x, degrees / 2.0, 0.5);
        return t >= 0 ? 1 - tail : tail;
    }

    // Φ(x) = ½ ± ½ P(½, x²/2), from the regularized incomplete gamma function, precise enough
    // for the Halley step in NormalQuantile.
    private static double NormalCdf(double x)
    {
        var half = 0.5 * RegularizedGammaP(0.5, x * x / 2);
        return x >= 0 ? 0.5 + half : 0.5 - half;
    }

    private static double RegularizedGammaP(double a, double x)
    {
        if (x <= 0)
        {
            return 0;
        }
        var logPrefix = a * Math.Log(x) - x - LogGamma(a);
        if (x < a + 1)
        {
            double term = 1 / a,
                sum = term;
            for (var n = 1; n < 1000; n++)
            {
                term *= x / (a + n);
                sum += term;
                if (Math.Abs(term) < Math.Abs(sum) * 1e-16)
                {
                    break;
                }
            }
            return sum * Math.Exp(logPrefix);
        }
        // The continued fraction for Q(a, x) (modified Lentz).
        const double tiny = 1e-300;
        var b = x + 1 - a;
        var c = 1 / tiny;
        var d = 1 / b;
        var h = d;
        for (var i = 1; i < 1000; i++)
        {
            var an = -i * (i - a);
            b += 2;
            d = an * d + b;
            d = Math.Abs(d) < tiny ? tiny : d;
            c = b + an / c;
            c = Math.Abs(c) < tiny ? tiny : c;
            d = 1 / d;
            var step = d * c;
            h *= step;
            if (Math.Abs(step - 1) < 1e-16)
            {
                break;
            }
        }
        return 1 - Math.Exp(logPrefix) * h;
    }

    // I_x(a, b), the regularized incomplete beta function (continued fraction, modified Lentz).
    private static double RegularizedBeta(double x, double a, double b)
    {
        if (x <= 0)
        {
            return 0;
        }
        if (x >= 1)
        {
            return 1;
        }
        if (x > (a + 1) / (a + b + 2))
        {
            return 1 - RegularizedBeta(1 - x, b, a);
        }
        var logPrefix =
            LogGamma(a + b) - LogGamma(a) - LogGamma(b) + a * Math.Log(x) + b * Math.Log(1 - x);
        const double tiny = 1e-300;
        var c = 1.0;
        var d = 1 - (a + b) * x / (a + 1);
        d = Math.Abs(d) < tiny ? tiny : d;
        d = 1 / d;
        var h = d;
        for (var m = 1; m < 1000; m++)
        {
            var even = m * (b - m) * x / ((a + 2 * m - 1) * (a + 2 * m));
            d = 1 + even * d;
            d = Math.Abs(d) < tiny ? tiny : d;
            c = 1 + even / c;
            c = Math.Abs(c) < tiny ? tiny : c;
            d = 1 / d;
            h *= d * c;
            var odd = -(a + m) * (a + b + m) * x / ((a + 2 * m) * (a + 2 * m + 1));
            d = 1 + odd * d;
            d = Math.Abs(d) < tiny ? tiny : d;
            c = 1 + odd / c;
            c = Math.Abs(c) < tiny ? tiny : c;
            d = 1 / d;
            var step = d * c;
            h *= step;
            if (Math.Abs(step - 1) < 1e-16)
            {
                break;
            }
        }
        return Math.Exp(logPrefix) * h / a;
    }

    // ln Γ(x) for x > 0 (Lanczos, g = 7, nine coefficients; relative error near 1e-15).
    private static double LogGamma(double x)
    {
        double[] g =
        [
            0.99999999999980993,
            676.5203681218851,
            -1259.1392167224028,
            771.32342877765313,
            -176.61502916214059,
            12.507343278686905,
            -0.13857109526572012,
            9.9843695780195716e-6,
            1.5056327351493116e-7,
        ];
        if (x < 0.5)
        {
            return Math.Log(Math.PI / Math.Abs(Math.Sin(Math.PI * x))) - LogGamma(1 - x);
        }
        x -= 1;
        var sum = g[0];
        for (var i = 1; i < g.Length; i++)
        {
            sum += g[i] / (x + i);
        }
        var t = x + 7.5;
        return 0.5 * Math.Log(2 * Math.PI) + (x + 0.5) * Math.Log(t) - t + Math.Log(sum);
    }

    // xoshiro256** seeded through SplitMix64: a fixed algorithm, so a seed gives the same
    // stream in every process, unlike System.Random.
    public sealed class Rng
    {
        private ulong _s0,
            _s1,
            _s2,
            _s3;

        public Rng(ulong seed)
        {
            _s0 = SplitMix(ref seed);
            _s1 = SplitMix(ref seed);
            _s2 = SplitMix(ref seed);
            _s3 = SplitMix(ref seed);
        }

        public ulong NextUInt64()
        {
            var result = ulong.RotateLeft(_s1 * 5, 7) * 9;
            var t = _s1 << 17;
            _s2 ^= _s0;
            _s3 ^= _s1;
            _s1 ^= _s2;
            _s0 ^= _s3;
            _s2 ^= t;
            _s3 = ulong.RotateLeft(_s3, 45);
            return result;
        }

        // A uniform integer in [0, n), without modulo bias (Lemire's method).
        public int NextInt(int n)
        {
            ArgumentOutOfRangeException.ThrowIfLessThan(n, 1);
            var bound = (ulong)n;
            var product = (UInt128)NextUInt64() * bound;
            var low = (ulong)product;
            if (low < bound)
            {
                var threshold = (0 - bound) % bound;
                while (low < threshold)
                {
                    product = (UInt128)NextUInt64() * bound;
                    low = (ulong)product;
                }
            }
            return (int)(product >> 64);
        }

        // A uniform double in [0, 1) with 53 random bits.
        public double NextDouble() => (NextUInt64() >> 11) * (1.0 / (1UL << 53));

        private static ulong SplitMix(ref ulong state)
        {
            state += 0x9E37_79B9_7F4A_7C15UL;
            var z = state;
            z = (z ^ (z >> 30)) * 0xBF58_476D_1CE4_E5B9UL;
            z = (z ^ (z >> 27)) * 0x94D0_49BB_1331_11EBUL;
            return z ^ (z >> 31);
        }
    }
}
