namespace Casebox.Server.Features.Privacy;

// A person behind a data point: their token, and whether the roster mapped it.
public readonly record struct Person(string Token, bool Mapped);

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
        if (k < Orgs.OrgSettings.MinimumK) throw new ArgumentOutOfRangeException(nameof(k), $"k is at least {Orgs.OrgSettings.MinimumK}.");
        return rows
            .GroupBy(r => r.Key)
            .Select(g => new KGroup<TKey>(g.Key, g.Where(r => r.Person.Mapped).Select(r => r.Person.Token).Distinct(StringComparer.Ordinal).Count(), g.Count()))
            .Where(g => g.People >= k)
            .ToList();
    }
}
