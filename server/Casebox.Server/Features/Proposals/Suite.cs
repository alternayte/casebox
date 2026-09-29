using Deedbox;

namespace Casebox.Server.Features.Proposals;

public static class SuiteEvents
{
    public sealed record HoldoutQueried(string Proposal, string Evaluation, int Rotation);

    public sealed record Rotated(
        IReadOnlyList<string> ToHeldOut,
        IReadOnlyList<string> ToDev,
        int Rotation,
        DateTimeOffset At
    );
}

// A workspace's suite: its approved cases in their split, and the held-out budget of the current
// rotation (docs/specs/self-evolution.md, Suites).
public sealed record Suite(int Rotation, int Queries, DateTimeOffset Since) : IState<Suite>
{
    public const int DefaultBudget = 10;

    public static Suite Initial { get; } = new(1, 0, DateTimeOffset.MinValue);

    public static Suite Evolve(Suite s, object e) =>
        e switch
        {
            SuiteEvents.HoldoutQueried => s with { Queries = s.Queries + 1 },
            SuiteEvents.Rotated x => new Suite(x.Rotation, 0, x.At),
            _ => s,
        };

    public static string StreamId(string workspace) => $"suite:{workspace}";

    public bool Spent(int budget) => Queries >= budget;
}

// Held-out queries per rotation never exceed the budget; a rotation happens only once it is spent.
public static class SuiteDecider
{
    public static IEnumerable<object> Query(Suite s, string proposal, string evaluation, int budget)
    {
        if (s.Spent(budget))
            throw new DomainException(
                $"The held-out set answered {budget} gate queries in this rotation; it rotates before the next."
            );
        return [new SuiteEvents.HoldoutQueried(proposal, evaluation, s.Rotation)];
    }

    public static IEnumerable<object> Rotate(
        Suite s,
        IReadOnlyList<string> toHeldOut,
        IReadOnlyList<string> toDev,
        int budget,
        DateTimeOffset at
    )
    {
        if (!s.Spent(budget))
            throw new DomainException(
                "The held-out set rotates only after its query budget is spent."
            );
        return [new SuiteEvents.Rotated(toHeldOut, toDev, s.Rotation + 1, at)];
    }
}
