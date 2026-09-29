using System.Globalization;
using System.Text.Json;
using System.Text.RegularExpressions;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Orgs;

namespace Casebox.Server.Features.GitHub;

// Reads GitHub objects and turns them into tokenized snapshots, in memory. Nothing it returns
// holds a login, name or email of a person.
public sealed partial class GitHubReader(Identities identities)
{
    // Check runs are read for the last commits of a pull request only.
    private const int MaxCheckedCommits = 5;

    [GeneratedRegex(@"(?im)^co-authored-by:\s*(claude|codex|cursor|openai|anthropic)\b")]
    private static partial Regex AgentTrailer();

    [GeneratedRegex(@"(?i)\breverts\s+(?<repo>[\w.-]+/[\w.-]+)?#(?<n>\d+)")]
    private static partial Regex RevertsPr();

    [GeneratedRegex(@"(?i)this reverts commit (?<sha>[0-9a-f]{7,40})")]
    private static partial Regex RevertsCommit();

    public static (string Owner, string Name) Split(string repo)
    {
        var parts = repo.Split('/');
        return (parts[^2], parts[^1]);
    }

    public async Task<PullRequestSnapshot> PullAsync(
        GitHubClient client,
        string repo,
        int number,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var (owner, name) = Split(repo);
        var path = $"repos/{owner}/{name}/pulls/{number}";
        var pr = await client.GetAsync(path, ct);
        var updatedAt = pr.GetProperty("updated_at").GetDateTimeOffset();
        var period = Identities.PeriodOf(
            settings.PseudonymPeriod,
            pr.GetProperty("created_at").GetDateTimeOffset()
        );

        var files = new List<ChangedFile>();
        foreach (var f in await client.ListAsync($"{path}/files", null, 30, ct))
        {
            files.Add(
                new ChangedFile(
                    f.GetProperty("filename").GetString()!,
                    f.GetProperty("additions").GetInt32(),
                    f.GetProperty("deletions").GetInt32()
                )
            );
        }

        var commits = new List<PrCommit>();
        foreach (var c in await client.ListAsync($"{path}/commits", null, 10, ct))
        {
            var commit = c.GetProperty("commit");
            var message = commit.GetProperty("message").GetString();
            var at = commit.GetProperty("committer").GetProperty("date").GetDateTimeOffset();
            commits.Add(
                new PrCommit(
                    c.GetProperty("sha").GetString()!,
                    await identities.TokenizeExternalAsync(message, period, ct),
                    await CommitAuthorAsync(c, period, ct),
                    at,
                    message is not null && AgentTrailer().IsMatch(message)
                )
            );
        }

        var reviews = new List<PrReview>();
        foreach (var r in await client.ListAsync($"{path}/reviews", null, 10, ct))
            reviews.Add(
                new PrReview(
                    r.GetProperty("id").GetInt64(),
                    r.GetProperty("state").GetString()!,
                    await PersonAsync(r, "user", period, ct),
                    r.TryGetProperty("submitted_at", out var s)
                    && s.ValueKind == JsonValueKind.String
                        ? s.GetDateTimeOffset()
                        : null,
                    await identities.TokenizeExternalAsync(Text(r, "body"), period, ct)
                )
            );

        var comments = new List<PrReviewComment>();
        foreach (var c in await client.ListAsync($"{path}/comments", null, 10, ct))
            comments.Add(
                new PrReviewComment(
                    c.GetProperty("id").GetInt64(),
                    Long(c, "pull_request_review_id"),
                    Long(c, "in_reply_to_id"),
                    c.GetProperty("path").GetString()!,
                    Int(c, "line"),
                    Int(c, "original_line"),
                    Text(c, "commit_id"),
                    await PersonAsync(c, "user", period, ct),
                    c.GetProperty("created_at").GetDateTimeOffset(),
                    await identities.TokenizeExternalAsync(Text(c, "body"), period, ct),
                    Text(c, "original_commit_id")
                )
            );

        // The checks of the head and of the commits before it: a CI fix is a failure on one commit
        // and a pass on a later one (docs/specs/steering.md).
        var headSha = pr.GetProperty("head").GetProperty("sha").GetString()!;
        var checks = new List<PrCheck>();
        foreach (
            var sha in commits
                .Select(c => c.Sha)
                .Append(headSha)
                .Distinct()
                .TakeLast(MaxCheckedCommits)
        )
        {
            var runs = await client.GetAsync(
                $"repos/{owner}/{name}/commits/{sha}/check-runs?per_page=100",
                ct
            );
            foreach (var run in runs.GetProperty("check_runs").EnumerateArray())
                checks.Add(
                    new PrCheck(
                        run.GetProperty("name").GetString()!,
                        Text(run, "conclusion"),
                        sha,
                        run.TryGetProperty("completed_at", out var done)
                        && done.ValueKind == JsonValueKind.String
                            ? done.GetDateTimeOffset()
                            : null
                    )
                );
        }

        var title = Text(pr, "title");
        var body = Text(pr, "body");
        int? reverts = null;
        if (
            title?.StartsWith("Revert \"", StringComparison.Ordinal) == true
            && body is not null
            && RevertsPr().Match(body) is { Success: true } m
            && (
                !m.Groups["repo"].Success
                || $"github.com/{m.Groups["repo"].Value}".Equals(
                    repo,
                    StringComparison.OrdinalIgnoreCase
                )
            )
        )
            reverts = int.Parse(m.Groups["n"].Value, CultureInfo.InvariantCulture);

        DateTimeOffset? mergedAt =
            pr.TryGetProperty("merged_at", out var ma) && ma.ValueKind == JsonValueKind.String
                ? ma.GetDateTimeOffset()
                : null;
        var baseSha = pr.GetProperty("base").GetProperty("sha").GetString()!;

        return new PullRequestSnapshot(
            repo,
            number,
            await identities.TokenizeExternalAsync(title, period, ct),
            await identities.TokenizeExternalAsync(body, period, ct),
            pr.GetProperty("state").GetString()!,
            pr.TryGetProperty("draft", out var d) && d.GetBoolean(),
            pr.GetProperty("head").GetProperty("ref").GetString()!,
            pr.GetProperty("base").GetProperty("ref").GetString()!,
            headSha,
            baseSha,
            await PersonAsync(pr, "user", period, ct),
            pr.GetProperty("created_at").GetDateTimeOffset(),
            updatedAt,
            mergedAt,
            Text(pr, "merge_commit_sha"),
            pr.GetProperty("labels")
                .EnumerateArray()
                .Select(l => l.GetProperty("name").GetString()!)
                .ToList(),
            files,
            commits,
            reviews,
            comments,
            checks,
            reverts
        );
    }

    public async Task<IssueSnapshot> IssueAsync(
        JsonElement issue,
        string repo,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var created = issue.GetProperty("created_at").GetDateTimeOffset();
        var period = Identities.PeriodOf(settings.PseudonymPeriod, created);
        return new IssueSnapshot(
            repo,
            issue.GetProperty("number").GetInt32(),
            await identities.TokenizeExternalAsync(Text(issue, "title"), period, ct),
            await identities.TokenizeExternalAsync(Text(issue, "body"), period, ct),
            issue.GetProperty("state").GetString()!,
            issue
                .GetProperty("labels")
                .EnumerateArray()
                .Select(l => l.GetProperty("name").GetString()!)
                .ToList(),
            await PersonAsync(issue, "assignee", period, ct),
            created,
            issue.TryGetProperty("closed_at", out var c) && c.ValueKind == JsonValueKind.String
                ? c.GetDateTimeOffset()
                : null,
            issue.GetProperty("updated_at").GetDateTimeOffset()
        );
    }

    // A commit that says "This reverts commit <sha>", with the pull request that merged the reverted commit.
    public async Task<RevertCommit?> RevertAsync(
        GitHubClient client,
        string repo,
        JsonElement commit,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var message = commit.GetProperty("commit").GetProperty("message").GetString();
        if (message is null || RevertsCommit().Match(message) is not { Success: true } m)
            return null;
        var (owner, name) = Split(repo);
        var reverted = m.Groups["sha"].Value;
        var pulls = await client.GetAsync($"repos/{owner}/{name}/commits/{reverted}/pulls", ct);
        int? pr =
            pulls.ValueKind == JsonValueKind.Array && pulls.GetArrayLength() > 0
                ? pulls[0].GetProperty("number").GetInt32()
                : null;
        var at = commit
            .GetProperty("commit")
            .GetProperty("committer")
            .GetProperty("date")
            .GetDateTimeOffset();
        var period = Identities.PeriodOf(settings.PseudonymPeriod, at);
        return new RevertCommit(
            repo,
            commit.GetProperty("sha").GetString()!,
            reverted,
            pr,
            at,
            await CommitAuthorAsync(commit, period, ct),
            await identities.TokenizeExternalAsync(message, period, ct)
        );
    }

    private async Task<PersonRef?> PersonAsync(
        JsonElement parent,
        string property,
        string period,
        CancellationToken ct
    ) =>
        parent.TryGetProperty(property, out var user) && user.ValueKind == JsonValueKind.Object
            ? await UserAsync(user, period, ct)
            : null;

    private async Task<PersonRef> UserAsync(JsonElement user, string period, CancellationToken ct)
    {
        var login = user.GetProperty("login").GetString()!;
        if (Text(user, "type") == "Bot" || login.EndsWith("[bot]", StringComparison.Ordinal))
            return new PersonRef($"bot:{login}", true, false);
        var (subject, mapped) = await identities.SubjectOfAsync("github", login, period, ct);
        return new PersonRef(subject, false, mapped);
    }

    private async Task<PersonRef?> CommitAuthorAsync(
        JsonElement commit,
        string period,
        CancellationToken ct
    )
    {
        if (commit.TryGetProperty("author", out var user) && user.ValueKind == JsonValueKind.Object)
            return await UserAsync(user, period, ct);
        var email = commit
            .GetProperty("commit")
            .GetProperty("author")
            .TryGetProperty("email", out var e)
            ? e.GetString()
            : null;
        if (string.IsNullOrEmpty(email))
            return null;
        var (subject, mapped) = await identities.SubjectOfAsync("email", email, period, ct);
        return new PersonRef(subject, false, mapped);
    }

    private static string? Text(JsonElement e, string property) =>
        e.TryGetProperty(property, out var v) && v.ValueKind == JsonValueKind.String
            ? v.GetString()
            : null;

    private static int? Int(JsonElement e, string property) =>
        e.TryGetProperty(property, out var v) && v.ValueKind == JsonValueKind.Number
            ? v.GetInt32()
            : null;

    private static long? Long(JsonElement e, string property) =>
        e.TryGetProperty(property, out var v) && v.ValueKind == JsonValueKind.Number
            ? v.GetInt64()
            : null;
}
