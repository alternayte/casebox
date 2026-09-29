using System.Text.RegularExpressions;

namespace Casebox.Server.Features.Privacy;

// Stored text keeps its person tokens, because erasure and the k rule need them. Text leaving the
// API never does: a token would let a reader follow one person across items (SDD section 11,
// "quotes without authors, tokens"). Every route that returns stored text masks it here.
public static partial class Masking
{
    [GeneratedRegex("person:[a-z2-7]{26}")]
    private static partial Regex Token();

    public static string? Mask(string? text) => text is null ? null : Token().Replace(text, "[person]");
}
