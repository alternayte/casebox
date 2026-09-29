namespace Casebox.Server.Features.Privacy;

// A person behind a data point: their token, whether the roster mapped it, and the pseudonym
// period the token belongs to.
public readonly record struct Person(string Token, bool Mapped, string Period = "");

// A group that may be shown: its key, how many distinct mapped people are behind it, and how many
// data points it holds. It never carries a token.
public sealed record KGroup<TKey>(TKey Key, int People, int Count);

// SDD section 11: a theme, quote or count is shown only when at least k distinct mapped people
// are behind it. Unmapped tokens never count toward k, because one person split into two tokens
// would let a group of two pass as three. Every report route builds its groups through this rule.
public static class KRule
{
    public static IReadOnlyList<KGroup<TKey>> Apply<TKey>(IEnumerable<(TKey Key, Person Person)> rows, int k)
        where TKey : notnull
    {
        RequireK(k);
        return rows
            .GroupBy(r => r.Key)
            .Select(g => new KGroup<TKey>(g.Key, People(g.Select(r => r.Person)), g.Count()))
            .Where(g => g.People >= k)
            .ToList();
    }

    // Distinct mapped people. A person has one token per period, so data spanning two periods
    // counts the largest number of distinct tokens in any one period: one person is never counted
    // twice.
    public static int People(IEnumerable<Person> people) =>
        people.Where(p => p.Mapped)
            .GroupBy(p => p.Period, StringComparer.Ordinal)
            .Select(g => g.Select(p => p.Token).Distinct(StringComparer.Ordinal).Count())
            .DefaultIfEmpty(0)
            .Max();

    public static bool Meets(IEnumerable<Person> people, int k)
    {
        RequireK(k);
        return People(people) >= k;
    }

    private static void RequireK(int k)
    {
        if (k < Orgs.OrgSettings.MinimumK) throw new ArgumentOutOfRangeException(nameof(k), $"k is at least {Orgs.OrgSettings.MinimumK}.");
    }
}
