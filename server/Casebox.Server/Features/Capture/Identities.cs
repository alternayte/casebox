using System.Text.RegularExpressions;

using Deedbox;

namespace Casebox.Server.Features.Capture;

// Replaces identity marks with subject IDs from the Deedbox pseudonymizer, in memory, before any
// write. An email the CLI did not mark is tokenized too, so no raw address is ever stored.
public sealed partial class Identities(IPseudonyms pseudonyms, Privacy.Roster roster, DeedboxContext context)
{
    [GeneratedRegex(@"⟦cbx:(?<kind>[a-z]+):(?<value>[^⟧]{1,320})⟧")]
    private static partial Regex Mark();

    [GeneratedRegex(@"⟦[^⟧]*⟧?")]
    private static partial Regex BrokenMark();

    [GeneratedRegex(@"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")]
    private static partial Regex Email();

    // An @mention in GitHub or Jira text: after a space or the start, not a decorator call.
    [GeneratedRegex(@"(?<=^|[\s(\[,])@(?<login>[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))(?![A-Za-z0-9(.-])")]
    private static partial Regex Mention();

    // The subject of one identity, and whether the roster mapped it.
    public async Task<(string Subject, bool Mapped)> SubjectOfAsync(string kind, string value, string period, CancellationToken ct)
    {
        var identity = Identity(kind, value) ?? throw new ArgumentException($"'{kind}' is not an identity kind.", nameof(kind));
        var canonical = await roster.CanonicalOfAsync(context.TenantId, identity, ct);
        return (await SubjectForAsync(canonical ?? identity, period, ct), canonical is not null);
    }

    // Text from GitHub or Jira: secrets redacted, and @mentions, emails and every name the roster
    // knows tokenized.
    public async Task<string?> TokenizeExternalAsync(string? text, string period, CancellationToken ct)
    {
        if (string.IsNullOrEmpty(text)) return text;
        var marked = Mention().Replace(Redaction.Redact(text), m => $"⟦cbx:github:{m.Groups["login"].Value}⟧");
        if (await NamesPatternAsync(ct) is { } names)
            marked = names.Replace(marked, m => $"⟦cbx:name:{m.Value}⟧");
        return await TokenizeAsync(marked, period, ct);
    }

    private Regex? _names;
    private bool _namesLoaded;

    // Whole-word matches of the roster's names, longest first, so "Ada Lovelace" wins over "Ada".
    private async Task<Regex?> NamesPatternAsync(CancellationToken ct)
    {
        if (_namesLoaded) return _names;
        var names = (await roster.NamesAsync(context.TenantId, ct)).Where(n => n.Length >= 3).OrderByDescending(n => n.Length).Select(Regex.Escape).ToList();
        _names = names.Count == 0 ? null : new Regex($@"(?<![\w⟦:])(?:{string.Join('|', names)})(?![\w⟧])");
        _namesLoaded = true;
        return _names;
    }

    private readonly Dictionary<string, string> _cache = new(StringComparer.Ordinal);

    public static string PeriodOf(Orgs.PseudonymPeriod period, DateTimeOffset at) => period switch
    {
        Orgs.PseudonymPeriod.Month => Deedbox.PseudonymPeriod.Month(at),
        Orgs.PseudonymPeriod.Year => at.UtcDateTime.Year.ToString(System.Globalization.CultureInfo.InvariantCulture),
        _ => Deedbox.PseudonymPeriod.Quarter(at),
    };

    // The subject of one identity mark, for fields that hold exactly one person, and whether the
    // roster mapped it. Unmapped subjects never count toward k.
    public async Task<(string Subject, bool Mapped)?> SubjectOfMarkAsync(string? mark, string period, CancellationToken ct)
    {
        if (string.IsNullOrEmpty(mark)) return null;
        var match = Mark().Match(mark);
        if (!match.Success || match.Length != mark.Length) return null;
        var identity = Identity(match.Groups["kind"].Value, match.Groups["value"].Value);
        if (identity is null) return null;
        var canonical = await roster.CanonicalOfAsync(context.TenantId, identity, ct);
        return (await SubjectForAsync(canonical ?? identity, period, ct), canonical is not null);
    }

    public async Task<string> TokenizeAsync(string text, string period, CancellationToken ct)
    {
        text = await ReplaceAsync(Mark(), text, m => SubjectAsync(m.Groups["kind"].Value, m.Groups["value"].Value, period, ct));
        text = BrokenMark().Replace(text, "[identity]");
        return await ReplaceAsync(Email(), text, m => SubjectAsync("email", m.Value, period, ct));
    }

    private async Task<string> SubjectAsync(string kind, string value, string period, CancellationToken ct)
    {
        var identity = Identity(kind, value);
        if (identity is null) return "[identity]";
        var canonical = await roster.CanonicalOfAsync(context.TenantId, identity, ct);
        return await SubjectForAsync(canonical ?? identity, period, ct);
    }

    public static string? Identity(string kind, string value) =>
        kind is "email" or "github" or "account" or "name" or "jira" && value.Trim().Length > 0
            ? Privacy.Roster.Normalize($"{kind}:{value}")
            : null;

    private async Task<string> SubjectForAsync(string identity, string period, CancellationToken ct)
    {
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
