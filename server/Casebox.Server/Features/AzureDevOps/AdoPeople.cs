using System.Net;
using System.Text.Json;

namespace Casebox.Server.Features.AzureDevOps;

// What Casebox reads of one Azure DevOps identity, in memory only: the mail and display name link
// it to the person's git email and name; neither is ever stored.
public sealed record AdoIdentity(string Id, string? Mail, string? DisplayName, bool Group);

// Resolves identity IDs through _apis/identities, 50 at a time, once per client. Identities that
// the server does not return, or that the token may not read, stay unknown.
public sealed class AdoPeople(AdoClient client)
{
    private const int Batch = 50;
    private readonly Dictionary<string, AdoIdentity?> _known = new(
        StringComparer.OrdinalIgnoreCase
    );

    // The token lacks Identity (Read): every lookup is unknown, and nobody is taken for a bot.
    public bool Refused { get; private set; }

    public async Task<AdoIdentity?> GetAsync(string id, CancellationToken ct)
    {
        if (!_known.ContainsKey(id))
            await LoadAsync([id], ct);
        return _known.GetValueOrDefault(id);
    }

    public async Task LoadAsync(IEnumerable<string> ids, CancellationToken ct)
    {
        var missing = ids.Where(i => !_known.ContainsKey(i))
            .Distinct(StringComparer.OrdinalIgnoreCase)
            .ToList();
        foreach (var chunk in missing.Chunk(Batch))
        {
            foreach (var id in chunk)
                _known[id] = null;
            if (Refused)
                continue;
            JsonElement body;
            try
            {
                body = await client.GetAsync(
                    $"_apis/identities?identityIds={string.Join(',', chunk)}&queryMembership=None",
                    ct
                );
            }
            catch (HttpRequestException e)
                when (e.StatusCode is HttpStatusCode.Unauthorized or HttpStatusCode.Forbidden)
            {
                Refused = true;
                continue;
            }

            foreach (var identity in body.GetProperty("value").EnumerateArray())
            {
                if (identity.ValueKind != JsonValueKind.Object)
                    continue;
                var id = identity.GetProperty("id").GetString()!;
                _known[id] = new AdoIdentity(
                    id,
                    Property(identity, "Mail"),
                    Text(identity, "customDisplayName") ?? Text(identity, "providerDisplayName"),
                    identity.TryGetProperty("isContainer", out var c) && c.GetBoolean()
                );
            }
        }
    }

    // A build service or another service identity, from the identity reference alone.
    public static bool IsService(JsonElement identityRef)
    {
        var unique = Text(identityRef, "uniqueName") ?? "";
        var display = Text(identityRef, "displayName") ?? "";
        var descriptor = Text(identityRef, "descriptor") ?? "";
        return unique.StartsWith(@"Build\", StringComparison.OrdinalIgnoreCase)
            || descriptor.StartsWith("svc.", StringComparison.Ordinal)
            || descriptor.StartsWith("s2s.", StringComparison.Ordinal)
            || display.Contains("Build Service", StringComparison.Ordinal)
            || (identityRef.TryGetProperty("isContainer", out var c) && c.GetBoolean());
    }

    private static string? Property(JsonElement identity, string name) =>
        identity.TryGetProperty("properties", out var p)
        && p.ValueKind == JsonValueKind.Object
        && p.TryGetProperty(name, out var v)
        && v.ValueKind == JsonValueKind.Object
        && v.TryGetProperty("$value", out var value)
        && value.ValueKind == JsonValueKind.String
        && value.GetString() is { Length: > 0 } s
            ? s
            : null;

    internal static string? Text(JsonElement e, string property) =>
        e.ValueKind == JsonValueKind.Object
        && e.TryGetProperty(property, out var v)
        && v.ValueKind == JsonValueKind.String
            ? v.GetString()
            : null;
}
