using Microsoft.Extensions.Caching.Memory;

namespace Casebox.Server.Features.Privacy;

// One person as a source knows them: a canonical identity, such as github:alice, and every other
// identity of the same person, such as email:alice@example.com or jira:JIRAUSER1234.
public sealed record RosterPerson(string Canonical, IReadOnlyList<string> Identities);

// Where the roster comes from: the GitHub organisation and the Jira project (step 6).
public interface IRosterSource
{
    Task<IReadOnlyList<RosterPerson>> PeopleAsync(string orgId, CancellationToken ct);
}

// Maps each identity to one canonical identity, so one person has one token. The roster is built
// from the sources when needed and kept in memory only, for 15 minutes; it is never stored.
public sealed class Roster(IEnumerable<IRosterSource> sources, IMemoryCache cache, ILogger<Roster> logger)
{
    private static readonly TimeSpan Lifetime = TimeSpan.FromMinutes(15);

    // The canonical identity of an identity the roster knows, or null.
    public async Task<string?> CanonicalOfAsync(string orgId, string identity, CancellationToken ct) =>
        (await MapAsync(orgId, ct)).GetValueOrDefault(identity);

    // Every person's name the roster knows, for tokenizing names in free text.
    public async Task<IReadOnlyList<string>> NamesAsync(string orgId, CancellationToken ct) =>
        (await MapAsync(orgId, ct)).Keys.Where(k => k.StartsWith("name:", StringComparison.Ordinal)).Select(k => k["name:".Length..]).ToList();

    // Every identity of the person behind an identity, the identity itself included.
    public async Task<IReadOnlyList<string>> AliasesOfAsync(string orgId, string identity, CancellationToken ct)
    {
        var map = await MapAsync(orgId, ct);
        var canonical = map.GetValueOrDefault(identity);
        if (canonical is null) return [identity];
        return [.. map.Where(e => e.Value == canonical).Select(e => e.Key).Append(identity).Append(canonical).Distinct(StringComparer.Ordinal)];
    }

    private async Task<Dictionary<string, string>> MapAsync(string orgId, CancellationToken ct)
    {
        if (cache.TryGetValue(Key(orgId), out Dictionary<string, string>? cached) && cached is not null) return cached;

        var people = new List<RosterPerson>();
        foreach (var source in sources)
        {
            try
            {
                people.AddRange(await source.PeopleAsync(orgId, ct));
            }
            catch (Exception e) when (e is HttpRequestException or TaskCanceledException)
            {
                // A source that cannot be reached leaves its people unmapped; they then count toward
                // no k until it answers again.
                logger.LogWarning(e, "A roster source could not be read; its people stay unmapped for now.");
            }
        }

        var map = Merge(people);
        cache.Set(Key(orgId), map, Lifetime);
        return map;
    }

    // People from different sources who share an identity (a GitHub login and a Jira account with
    // one email) are one person. The canonical identity prefers GitHub, then Jira, then email.
    public static Dictionary<string, string> Merge(IEnumerable<RosterPerson> people)
    {
        var parent = new Dictionary<string, string>(StringComparer.Ordinal);
        string Find(string x)
        {
            while (parent[x] != x) x = parent[x] = parent[parent[x]];
            return x;
        }

        foreach (var person in people)
        {
            var ids = person.Identities.Append(person.Canonical).Select(Normalize).Distinct(StringComparer.Ordinal).ToList();
            foreach (var id in ids) parent.TryAdd(id, id);
            foreach (var id in ids.Skip(1))
            {
                var (a, b) = (Find(ids[0]), Find(id));
                if (a != b) parent[b] = a;
            }
        }

        static int Rank(string id) => id.StartsWith("github:", StringComparison.Ordinal) ? 0 : id.StartsWith("jira:", StringComparison.Ordinal) ? 1 : id.StartsWith("email:", StringComparison.Ordinal) ? 2 : 3;
        var canonical = parent.Keys.GroupBy(Find).ToDictionary(g => g.Key, g => g.OrderBy(Rank).ThenBy(x => x, StringComparer.Ordinal).First());
        return parent.Keys.ToDictionary(id => id, id => canonical[Find(id)], StringComparer.Ordinal);
    }

    public static string Normalize(string identity)
    {
        var colon = identity.IndexOf(':', StringComparison.Ordinal);
        if (colon <= 0) return identity.Trim();
        var kind = identity[..colon].Trim().ToLowerInvariant();
        var value = identity[(colon + 1)..].Trim();
        return kind is "name" ? $"{kind}:{value}" : $"{kind}:{value.ToLowerInvariant()}";
    }

    private static string Key(string orgId) => $"roster:{orgId}";
}
