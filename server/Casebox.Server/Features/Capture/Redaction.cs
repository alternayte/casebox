using System.Text.RegularExpressions;

namespace Casebox.Server.Features.Capture;

// The server repeats the CLI's redaction before any write, so a stale or modified CLI cannot
// store a secret. The rules match cli/internal/capture/redact.go; checks/redaction-rules.sh keeps
// the two lists the same.
public static partial class Redaction
{
    private static readonly (string Name, Regex Pattern)[] Rules =
    [
        ("private_key", PrivateKey()),
        ("aws_key", AwsKey()),
        ("github_token", GitHubToken()),
        ("slack_token", SlackToken()),
        ("anthropic_key", AnthropicKey()),
        ("openai_key", OpenAiKey()),
        ("jwt", Jwt()),
        ("bearer", Bearer()),
        ("url_credentials", UrlCredentials()),
        ("assignment", Assignment()),
    ];

    public static string Redact(string text)
    {
        foreach (var (name, pattern) in Rules)
        {
            text = name switch
            {
                "assignment" => pattern.Replace(text, m => $"{m.Groups["key"].Value}{m.Groups["sep"].Value}[redacted:{name}]"),
                "url_credentials" => pattern.Replace(text, m => $"{m.Groups["scheme"].Value}[redacted:{name}]@"),
                "bearer" => pattern.Replace(text, m => $"{m.Groups["prefix"].Value}[redacted:{name}]"),
                _ => pattern.Replace(text, $"[redacted:{name}]"),
            };
        }

        return HomePath().Replace(text, "~");
    }

    [GeneratedRegex(@"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----")]
    private static partial Regex PrivateKey();

    [GeneratedRegex(@"\b(AKIA|ASIA)[0-9A-Z]{16}\b")]
    private static partial Regex AwsKey();

    [GeneratedRegex(@"\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b")]
    private static partial Regex GitHubToken();

    [GeneratedRegex(@"\bxox[abposr]-[A-Za-z0-9-]{10,}\b")]
    private static partial Regex SlackToken();

    [GeneratedRegex(@"\bsk-ant-[A-Za-z0-9_-]{20,}\b")]
    private static partial Regex AnthropicKey();

    [GeneratedRegex(@"\bsk-(proj-)?[A-Za-z0-9_-]{20,}\b")]
    private static partial Regex OpenAiKey();

    [GeneratedRegex(@"\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b")]
    private static partial Regex Jwt();

    [GeneratedRegex(@"(?<prefix>\b[Bb]earer\s+)[A-Za-z0-9._~+/=-]{16,}")]
    private static partial Regex Bearer();

    [GeneratedRegex(@"(?<scheme>\b[a-z][a-z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@")]
    private static partial Regex UrlCredentials();

    [GeneratedRegex(@"(?i)(?<key>\b[\w.-]*(password|passwd|secret|token|key)[\w.-]*)(?<sep>[""']?\s*[:=]\s*[""']?)(?!\[redacted:)[^\s""',;]{4,}")]
    private static partial Regex Assignment();

    [GeneratedRegex(@"(/Users/|/home/|[A-Z]:\\Users\\)[^/\\\s""']+")]
    private static partial Regex HomePath();
}
