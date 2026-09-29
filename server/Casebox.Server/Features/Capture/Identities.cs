using System.Text.RegularExpressions;

using Deedbox;

namespace Casebox.Server.Features.Capture;

// Replaces identity marks with subject IDs from the Deedbox pseudonymizer, in memory, before any
// write. An email the CLI did not mark is tokenized too, so no raw address is ever stored.
public sealed partial class Identities(IPseudonyms pseudonyms)
{
    [GeneratedRegex(@"⟦cbx:(?<kind>[a-z]+):(?<value>[^⟧]{1,320})⟧")]
    private static partial Regex Mark();

    [GeneratedRegex(@"⟦[^⟧]*⟧?")]
    private static partial Regex BrokenMark();

    [GeneratedRegex(@"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")]
    private static partial Regex Email();

    private readonly Dictionary<string, string> _cache = new(StringComparer.Ordinal);

    public static string PeriodOf(Orgs.PseudonymPeriod period, DateTimeOffset at) => period switch
    {
        Orgs.PseudonymPeriod.Month => Deedbox.PseudonymPeriod.Month(at),
        Orgs.PseudonymPeriod.Year => at.UtcDateTime.Year.ToString(System.Globalization.CultureInfo.InvariantCulture),
        _ => Deedbox.PseudonymPeriod.Quarter(at),
    };

    // The subject of one identity mark, for fields that hold exactly one person.
    public async Task<string?> SubjectOfMarkAsync(string? mark, string period, CancellationToken ct)
    {
        if (string.IsNullOrEmpty(mark)) return null;
        var match = Mark().Match(mark);
        return match.Success && match.Length == mark.Length
            ? await SubjectAsync(match.Groups["kind"].Value, match.Groups["value"].Value, period, ct)
            : null;
    }

    public async Task<string> TokenizeAsync(string text, string period, CancellationToken ct)
    {
        text = await ReplaceAsync(Mark(), text, m => SubjectAsync(m.Groups["kind"].Value, m.Groups["value"].Value, period, ct));
        text = BrokenMark().Replace(text, "[identity]");
        return await ReplaceAsync(Email(), text, m => SubjectAsync("email", m.Value, period, ct));
    }

    private async Task<string> SubjectAsync(string kind, string value, string period, CancellationToken ct)
    {
        var identity = kind switch
        {
            "email" or "github" or "account" => $"{kind}:{value.Trim().ToLowerInvariant()}",
            "name" => $"name:{value.Trim()}",
            _ => null,
        };
        if (identity is null || identity.Length <= kind.Length + 1) return "[identity]";

        var key = $"{period}|{identity}";
        if (!_cache.TryGetValue(key, out var subject))
        {
            subject = await pseudonyms.SubjectForAsync(identity, period, ct);
            _cache[key] = subject;
        }

        return subject;
    }

    private static async Task<string> ReplaceAsync(Regex pattern, string text, Func<Match, Task<string>> replace)
    {
        var matches = pattern.Matches(text);
        if (matches.Count == 0) return text;
        var result = new System.Text.StringBuilder();
        var last = 0;
        foreach (Match match in matches)
        {
            result.Append(text, last, match.Index - last).Append(await replace(match));
            last = match.Index + match.Length;
        }

        return result.Append(text, last, text.Length - last).ToString();
    }
}
