using System.Text.RegularExpressions;

namespace Casebox.Server.Features.WorkItems;

// Finds work-item keys in branch names and pull request text: Jira keys of the configured
// projects, and GitHub issue numbers.
public sealed partial class WorkItemKeys(IReadOnlyList<string> jiraProjects)
{
    [GeneratedRegex(@"(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:(?<repo>[\w.-]+/[\w.-]+))?#(?<n>\d+)\b")]
    private static partial Regex ClosingKeyword();

    [GeneratedRegex(@"(?i)(?:^|[/_-])(?:gh|issue|issues)[-_]?(?<n>\d+)\b|^(?:[\w-]+/)?(?<n>\d+)[-_]")]
    private static partial Regex BranchIssue();

    private readonly Regex? _jira = jiraProjects.Count == 0
        ? null
        : new Regex($@"(?i)(?<![A-Za-z0-9])(?<key>(?:{string.Join('|', jiraProjects.Select(Regex.Escape))})-\d+)(?![0-9])", RegexOptions.Compiled);

    // Jira keys anywhere, as stream IDs.
    public IEnumerable<string> Jira(string? text) =>
        _jira is null || string.IsNullOrEmpty(text)
            ? []
            : _jira.Matches(text).Select(m => WorkItem.JiraStream(m.Groups["key"].Value.ToUpperInvariant())).Distinct();

    // Work items a branch name names: Jira keys, or a GitHub issue number such as gh-12 or 12-fix-login.
    public IEnumerable<string> FromBranch(string? branch, string repo) =>
        string.IsNullOrEmpty(branch)
            ? []
            : Jira(branch).Concat(BranchIssue().Matches(branch).Select(m => WorkItem.GitHubStream(repo, int.Parse(m.Groups["n"].Value, System.Globalization.CultureInfo.InvariantCulture)))).Distinct();

    // Work items a pull request names in its title or body: Jira keys, and issues it closes.
    public IEnumerable<string> FromPullRequest(string? title, string? body, string repo) =>
        Jira($"{title}\n{body}").Concat(ClosingKeyword().Matches($"{title}\n{body}").Select(m =>
            WorkItem.GitHubStream(m.Groups["repo"].Success ? $"github.com/{m.Groups["repo"].Value.ToLowerInvariant()}" : repo,
                int.Parse(m.Groups["n"].Value, System.Globalization.CultureInfo.InvariantCulture)))).Distinct();

    // What casebox link or CASEBOX_WORK_ITEM named: a Jira key or an issue number.
    public string? FromExplicit(string? key, string? repo)
    {
        if (string.IsNullOrWhiteSpace(key)) return null;
        var trimmed = key.Trim();
        if (Jira(trimmed).FirstOrDefault() is { } jira) return jira;
        return repo is not null && int.TryParse(trimmed.TrimStart('#'), System.Globalization.NumberStyles.None, System.Globalization.CultureInfo.InvariantCulture, out var n)
            ? WorkItem.GitHubStream(repo, n)
            : null;
    }
}
