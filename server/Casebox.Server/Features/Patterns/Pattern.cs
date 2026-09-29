using System.Collections.Immutable;
using System.Security.Cryptography;
using System.Text;
using Deedbox;

namespace Casebox.Server.Features.Patterns;

public enum PatternStatus
{
    Open,
    Acknowledged,
    Dismissed,
    Resolved,
}

// The group a pattern belongs to: what went wrong (with its free label for "other"), the
// prevention and the top-level path (docs/specs/self-evolution.md, Groups).
public sealed record PatternKey(string WentWrong, string? Label, string Prevention, string Path)
{
    public string Hash =>
        Convert.ToHexStringLower(
            SHA256.HashData(Encoding.UTF8.GetBytes($"{WentWrong}|{Label}|{Prevention}|{Path}"))
        )[..20];
}

public static class PatternEvents
{
    public sealed record Detected(
        string Workspace,
        string WentWrong,
        string? Label,
        string Prevention,
        string Path,
        string Title,
        string Summary,
        IReadOnlyList<string> Refs,
        bool Advisory
    );

    public sealed record CorrectionsAdded(IReadOnlyList<string> Refs);

    public sealed record Acknowledged(string By);

    public sealed record Dismissed(string By, string Reason);

    public sealed record Resolved(string Proposal, double Before, double After);

    public sealed record Reopened(string Reason);
}

public sealed record Pattern(
    bool Exists,
    string Workspace,
    PatternKey? Key,
    PatternStatus Status,
    ImmutableHashSet<string> Refs,
    bool Advisory
) : IState<Pattern>
{
    public static Pattern Initial { get; } =
        new(false, "", null, PatternStatus.Open, ImmutableHashSet<string>.Empty, false);

    public static Pattern Evolve(Pattern s, object e) =>
        e switch
        {
            PatternEvents.Detected x => s with
            {
                Exists = true,
                Workspace = x.Workspace,
                Key = new PatternKey(x.WentWrong, x.Label, x.Prevention, x.Path),
                Refs = [.. x.Refs],
                Advisory = x.Advisory,
            },
            PatternEvents.CorrectionsAdded x => s with { Refs = s.Refs.Union(x.Refs) },
            PatternEvents.Acknowledged => s with { Status = PatternStatus.Acknowledged },
            PatternEvents.Dismissed => s with { Status = PatternStatus.Dismissed },
            PatternEvents.Resolved => s with { Status = PatternStatus.Resolved },
            PatternEvents.Reopened => s with { Status = PatternStatus.Open },
            _ => s,
        };

    public static string StreamId(string id) => $"pattern:{id}";

    // A pattern's ID comes from what detected it, so detecting the same part twice appends nothing.
    public static string IdFor(
        string org,
        string workspace,
        PatternKey key,
        IEnumerable<string> refs
    ) =>
        Convert.ToHexStringLower(
            SHA256.HashData(
                Encoding.UTF8.GetBytes(
                    $"{org}|{workspace}|{key.Hash}|{string.Join(",", refs.Order(StringComparer.Ordinal))}"
                )
            )
        )[..20];

    // The prevention classes Casebox does not change by pull request (SDD section 9).
    public static bool IsAdvisory(string prevention) =>
        prevention is "clearer_ticket" or "stronger_model" or "tool_access";

    public bool Proposable =>
        Exists && !Advisory && Status is PatternStatus.Open or PatternStatus.Acknowledged;
}

// The rules of the pattern stream: detected once, no new corrections or acknowledgement after a
// dismissal, a dismissal needs a reason, resolved needs a fall in the rate, reopened only after it.
public static class PatternDecider
{
    public static IEnumerable<object> Detect(Pattern p, PatternEvents.Detected detected)
    {
        if (p.Exists)
            return [];
        if (detected.Refs.Count == 0)
            throw new DomainException("A pattern needs its corrections.");
        return [detected];
    }

    public static IEnumerable<object> AddCorrections(Pattern p, IReadOnlyList<string> refs)
    {
        Require(p);
        if (p.Status == PatternStatus.Dismissed)
            return [];
        var added = refs.Where(r => !p.Refs.Contains(r)).Distinct().ToList();
        return added.Count == 0 ? [] : [new PatternEvents.CorrectionsAdded(added)];
    }

    public static IEnumerable<object> Acknowledge(Pattern p, string by)
    {
        Require(p);
        return p.Status switch
        {
            PatternStatus.Open => [new PatternEvents.Acknowledged(by)],
            PatternStatus.Dismissed => throw new DomainException(
                "A dismissed pattern cannot be acknowledged."
            ),
            _ => [],
        };
    }

    public static IEnumerable<object> Dismiss(Pattern p, string by, string reason)
    {
        Require(p);
        if (string.IsNullOrWhiteSpace(reason))
            throw new DomainException("A dismissal needs a reason.");
        return p.Status == PatternStatus.Dismissed
            ? []
            : [new PatternEvents.Dismissed(by, reason.Trim())];
    }

    public static IEnumerable<object> Resolve(
        Pattern p,
        string proposal,
        double before,
        double after
    )
    {
        Require(p);
        if (after >= before)
            throw new DomainException("A pattern is resolved only when its correction rate fell.");
        return p.Status is PatternStatus.Resolved or PatternStatus.Dismissed
            ? []
            : [new PatternEvents.Resolved(proposal, before, after)];
    }

    public static IEnumerable<object> Reopen(Pattern p, string reason)
    {
        Require(p);
        return p.Status == PatternStatus.Resolved ? [new PatternEvents.Reopened(reason)] : [];
    }

    private static void Require(Pattern p)
    {
        if (!p.Exists)
            throw new NotFoundException("The pattern does not exist.");
    }
}
