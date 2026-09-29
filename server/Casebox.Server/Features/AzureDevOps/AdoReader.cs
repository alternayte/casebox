using System.Globalization;
using System.Text.Json;
using System.Text.RegularExpressions;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Orgs;

namespace Casebox.Server.Features.AzureDevOps;

// One Azure DevOps repository as the reader needs it: the Casebox name, and the URL path of the
// repository under its collection.
public sealed record AdoRepo(string Name, string Project, string Repository)
{
    public string Api => $"{Project}/_apis/git/repositories/{Repository}";
}

// Reads Azure DevOps pull requests and commits into the tokenized snapshots GitHub polling makes,
// in memory. Nothing it returns holds an identity ID, account, mail or display name.
public sealed partial class AdoReader(Identities identities)
{
    // The build policy: its evaluations are the pull request's CI.
    private const string BuildPolicy = "0609b952-1397-4640-95ec-e00a01b2c241";

    [GeneratedRegex(@"(?im)^co-authored-by:\s*(claude|codex|cursor|openai|anthropic)\b")]
    private static partial Regex AgentTrailer();

    // The default merge commit message of a completed pull request.
    [GeneratedRegex(@"^Merged PR (?<n>\d+):")]
    private static partial Regex MergedPr();

    [GeneratedRegex(@"\bMerged PR (?<n>\d+)\b")]
    private static partial Regex NamesMergedPr();

    [GeneratedRegex(@"(?i)this reverts commit (?<sha>[0-9a-f]{7,40})")]
    private static partial Regex RevertsCommit();

    [GeneratedRegex(@"(?i)\breverts?\b[^\n!]*!(?<n>\d+)")]
    private static partial Regex RevertsPr();

    // The pull request a "Merged PR <n>:" commit completed.
    public static int? MergedPrOf(string? message) =>
        message is not null && MergedPr().Match(message) is { Success: true } m
            ? int.Parse(m.Groups["n"].Value, CultureInfo.InvariantCulture)
            : null;

    public async Task<PullRequestSnapshot> PullAsync(
        AdoClient client,
        AdoPeople people,
        AdoRepo repo,
        JsonElement pr,
        string? projectId,
        IReadOnlyDictionary<int, string> mergeCommits,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var number = pr.GetProperty("pullRequestId").GetInt32();
        var path = $"{repo.Api}/pullRequests/{number}";
        var created = pr.GetProperty("creationDate").GetDateTimeOffset();
        var period = Identities.PeriodOf(settings.PseudonymPeriod, created);

        var iterations = (await client.GetAsync($"{path}/iterations", ct))
            .GetProperty("value")
            .EnumerateArray()
            .Select(i =>
                (
                    Id: i.GetProperty("id").GetInt32(),
                    Sha: CommitId(i, "sourceRefCommit"),
                    Created: Date(i, "createdDate"),
                    Updated: Date(i, "updatedDate")
                )
            )
            .OrderBy(i => i.Id)
            .ToList();
        var headSha = CommitId(pr, "lastMergeSourceCommit") ?? iterations.LastOrDefault().Sha ?? "";
        string ShaOfIteration(int? id) => iterations.FirstOrDefault(i => i.Id == id).Sha ?? headSha;
        string ShaAt(DateTimeOffset at) =>
            iterations.LastOrDefault(i => i.Created is { } c && c <= at).Sha ?? headSha;

        var files = new List<ChangedFile>();
        if (iterations.Count > 0)
        {
            var changes = await client.GetAsync(
                $"{path}/iterations/{iterations[^1].Id}/changes?$compareTo=0&$top=2000",
                ct
            );
            foreach (var change in changes.GetProperty("changeEntries").EnumerateArray())
                if (
                    change.TryGetProperty("item", out var item)
                    && AdoPeople.Text(item, "path") is { } file
                    && !(item.TryGetProperty("isFolder", out var folder) && folder.GetBoolean())
                )
                    files.Add(new ChangedFile(file.TrimStart('/'), 0, 0));
        }

        // Newest first from the server; oldest first in the snapshot, as GitHub lists them.
        var commits = new List<PrCommit>();
        foreach (
            var c in (await client.GetAsync($"{path}/commits?$top=250", ct))
                .GetProperty("value")
                .EnumerateArray()
                .Reverse()
        )
        {
            var sha = c.GetProperty("commitId").GetString()!;
            var message = await MessageAsync(client, repo, c, ct);
            commits.Add(
                new PrCommit(
                    sha,
                    await identities.TokenizeAdoAsync(message, period, ct),
                    await GitAuthorAsync(c, period, ct),
                    Date(c.GetProperty("committer"), "date") ?? created,
                    message is not null && AgentTrailer().IsMatch(message)
                )
            );
        }

        var threads = (await client.GetAsync($"{path}/threads", ct))
            .GetProperty("value")
            .EnumerateArray()
            .Where(t => !(t.TryGetProperty("isDeleted", out var d) && d.GetBoolean()))
            .ToList();
        var reviews = new List<PrReview>();
        var comments = new List<PrReviewComment>();
        foreach (var thread in threads)
        {
            var threadId = thread.GetProperty("id").GetInt64();
            var texts = thread
                .GetProperty("comments")
                .EnumerateArray()
                .Where(c =>
                    AdoPeople.Text(c, "commentType") is null or "text"
                    && !(c.TryGetProperty("isDeleted", out var d) && d.GetBoolean())
                )
                .OrderBy(c => c.GetProperty("id").GetInt64())
                .ToList();

            if (VoteOf(thread) is { } vote)
            {
                var voter = VoterOf(thread);
                if (voter is not null)
                    reviews.Add(
                        new PrReview(
                            threadId,
                            vote,
                            await PersonAsync(people, voter.Value, period, ct),
                            Date(thread, "publishedDate"),
                            null
                        )
                    );
                continue;
            }

            if (texts.Count == 0)
                continue;
            var context =
                thread.TryGetProperty("threadContext", out var tc)
                && tc.ValueKind == JsonValueKind.Object
                    ? tc
                    : (JsonElement?)null;
            var filePath = context is { } ctx ? AdoPeople.Text(ctx, "filePath") : null;
            if (filePath is null)
            {
                // A comment on the whole pull request is a review without a verdict.
                var first = texts[0];
                reviews.Add(
                    new PrReview(
                        threadId,
                        "COMMENTED",
                        await PersonAsync(people, first.GetProperty("author"), period, ct),
                        Date(first, "publishedDate"),
                        await identities.TokenizeAdoAsync(
                            AdoPeople.Text(first, "content"),
                            period,
                            ct
                        )
                    )
                );
                continue;
            }

            var line =
                Line(context!.Value, "rightFileStart") ?? Line(context.Value, "leftFileStart");
            int? iteration =
                thread.TryGetProperty("pullRequestThreadContext", out var ptc)
                && ptc.ValueKind == JsonValueKind.Object
                && ptc.TryGetProperty("iterationContext", out var ic)
                && ic.ValueKind == JsonValueKind.Object
                && ic.TryGetProperty("secondComparingIteration", out var second)
                && second.ValueKind == JsonValueKind.Number
                    ? second.GetInt32()
                    : null;
            var rootId = CommentId(threadId, texts[0].GetProperty("id").GetInt64());
            foreach (var c in texts)
            {
                var at = Date(c, "publishedDate") ?? created;
                var sha = iteration is not null ? ShaOfIteration(iteration) : ShaAt(at);
                var id = CommentId(threadId, c.GetProperty("id").GetInt64());
                comments.Add(
                    new PrReviewComment(
                        id,
                        null,
                        id == rootId ? null : rootId,
                        filePath.TrimStart('/'),
                        line,
                        line,
                        sha,
                        await PersonAsync(people, c.GetProperty("author"), period, ct),
                        at,
                        await identities.TokenizeAdoAsync(AdoPeople.Text(c, "content"), period, ct),
                        sha
                    )
                );
            }
        }

        var checks = new List<PrCheck>();
        var updated = new List<DateTimeOffset?> { created, Date(pr, "closedDate") };
        updated.AddRange(iterations.Select(i => i.Updated));
        updated.AddRange(threads.Select(t => Date(t, "lastUpdatedDate")));
        foreach (
            var status in (await client.GetAsync($"{path}/statuses", ct))
                .GetProperty("value")
                .EnumerateArray()
        )
        {
            var conclusion = AdoPeople.Text(status, "state") switch
            {
                "succeeded" => "success",
                "failed" or "error" => "failure",
                "notApplicable" => "skipped",
                _ => null,
            };
            var at = Date(status, "updatedDate") ?? Date(status, "creationDate");
            updated.Add(at);
            var context = status.GetProperty("context");
            var genre = AdoPeople.Text(context, "genre");
            var name = AdoPeople.Text(context, "name") ?? "status";
            int? iteration =
                status.TryGetProperty("iterationId", out var it)
                && it.ValueKind == JsonValueKind.Number
                    ? it.GetInt32()
                    : null;
            checks.Add(
                new PrCheck(
                    string.IsNullOrEmpty(genre) ? name : $"{genre}/{name}",
                    conclusion,
                    iteration is not null ? ShaOfIteration(iteration) : headSha,
                    conclusion is null ? null : at
                )
            );
        }

        if (projectId is not null)
        {
            var artifact = Uri.EscapeDataString(
                $"vstfs:///CodeReview/CodeReviewId/{projectId}/{number}"
            );
            foreach (
                var evaluation in (
                    await client.GetAsync(
                        $"{repo.Project}/_apis/policy/evaluations?artifactId={artifact}&api-version={AdoClient.PolicyApiVersion}",
                        ct
                    )
                )
                    .GetProperty("value")
                    .EnumerateArray()
            )
            {
                var configuration = evaluation.GetProperty("configuration");
                if (
                    AdoPeople.Text(configuration.GetProperty("type"), "id") is not { } type
                    || !type.Equals(BuildPolicy, StringComparison.OrdinalIgnoreCase)
                )
                    continue;
                var conclusion = AdoPeople.Text(evaluation, "status") switch
                {
                    "approved" => "success",
                    "rejected" or "broken" => "failure",
                    _ => null,
                };
                var done = Date(evaluation, "completedDate");
                updated.Add(done ?? Date(evaluation, "startedDate"));
                var name =
                    configuration.TryGetProperty("settings", out var s)
                    && AdoPeople.Text(s, "displayName") is { Length: > 0 } display
                        ? display
                        : "build";
                checks.Add(
                    new PrCheck(
                        $"build/{name}",
                        conclusion,
                        headSha,
                        conclusion is null ? null : done
                    )
                );
            }
        }

        var title = AdoPeople.Text(pr, "title");
        var body = AdoPeople.Text(pr, "description");
        var state = AdoPeople.Text(pr, "status");
        DateTimeOffset? mergedAt = state == "completed" ? Date(pr, "closedDate") : null;

        return new PullRequestSnapshot(
            repo.Name,
            number,
            await identities.TokenizeAdoAsync(title, period, ct),
            await identities.TokenizeAdoAsync(body, period, ct),
            state == "active" ? "open" : "closed",
            pr.TryGetProperty("isDraft", out var draft) && draft.GetBoolean(),
            Branch(AdoPeople.Text(pr, "sourceRefName")),
            Branch(AdoPeople.Text(pr, "targetRefName")),
            headSha,
            CommitId(pr, "lastMergeTargetCommit") ?? "",
            await PersonAsync(people, pr.GetProperty("createdBy"), period, ct),
            created,
            updated.Where(u => u is not null).Max()!.Value,
            mergedAt,
            mergedAt is null
                ? null
                : mergeCommits.GetValueOrDefault(number) ?? CommitId(pr, "lastMergeCommit"),
            pr.TryGetProperty("labels", out var labels) && labels.ValueKind == JsonValueKind.Array
                ? labels
                    .EnumerateArray()
                    .Where(l => !(l.TryGetProperty("active", out var a) && !a.GetBoolean()))
                    .Select(l => AdoPeople.Text(l, "name"))
                    .OfType<string>()
                    .ToList()
                : [],
            files,
            commits,
            reviews,
            comments,
            checks,
            RevertedBy(title, body, commits)
        );
    }

    // A commit on the default branch that reverts a completed pull request: "Revert" and the
    // "Merged PR <n>" it undoes, or "This reverts commit <sha>" of such a merge commit. A revert
    // that a pull request merged is read from that pull request instead.
    public async Task<RevertCommit?> RevertAsync(
        AdoClient client,
        AdoRepo repo,
        JsonElement commit,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var first = AdoPeople.Text(commit, "comment");
        if (
            first is null
            || MergedPrOf(first) is not null
            || !first.StartsWith("Revert", StringComparison.OrdinalIgnoreCase)
        )
            return null;
        var message = await MessageAsync(client, repo, commit, ct);
        int? reverted = null;
        var revertedSha = "";
        if (NamesMergedPr().Match(message!) is { Success: true } named)
            reverted = int.Parse(named.Groups["n"].Value, CultureInfo.InvariantCulture);
        if (RevertsCommit().Match(message!) is { Success: true } m)
        {
            revertedSha = m.Groups["sha"].Value;
            if (reverted is null)
            {
                var target = await client.GetAsync($"{repo.Api}/commits/{revertedSha}", ct);
                reverted = MergedPrOf(AdoPeople.Text(target, "comment"));
            }
        }

        if (reverted is null)
            return null;
        var at = Date(commit.GetProperty("committer"), "date")!.Value;
        var period = Identities.PeriodOf(settings.PseudonymPeriod, at);
        return new RevertCommit(
            repo.Name,
            commit.GetProperty("commitId").GetString()!,
            revertedSha,
            reverted,
            at,
            await GitAuthorAsync(commit, period, ct),
            await identities.TokenizeAdoAsync(message, period, ct)
        );
    }

    // A pull request made with Revert names the pull request it undoes in its description (!n),
    // or its commits name the merge commit ("Merged PR <n>").
    private static int? RevertedBy(string? title, string? body, IReadOnlyList<PrCommit> commits)
    {
        if (title?.StartsWith("Revert", StringComparison.OrdinalIgnoreCase) != true)
            return null;
        foreach (var text in new[] { $"{title}\n{body}" }.Concat(commits.Select(c => c.Message)))
        {
            if (text is null)
                continue;
            if (NamesMergedPr().Match(text) is { Success: true } merged)
                return int.Parse(merged.Groups["n"].Value, CultureInfo.InvariantCulture);
            if (RevertsPr().Match(text) is { Success: true } named)
                return int.Parse(named.Groups["n"].Value, CultureInfo.InvariantCulture);
        }

        return null;
    }

    // A person as an identity reference names them: a subject ID, or a bot. Build services, groups
    // and identities without mail are bots and never count toward k.
    private async Task<PersonRef?> PersonAsync(
        AdoPeople people,
        JsonElement identityRef,
        string period,
        CancellationToken ct
    )
    {
        if (
            identityRef.ValueKind != JsonValueKind.Object
            || AdoPeople.Text(identityRef, "id") is not { } id
        )
            return null;
        var identity = await people.GetAsync(id, ct);
        var bot =
            AdoPeople.IsService(identityRef)
            || identity is { Group: true }
            || (identity is not null && identity.Mail is null);
        var (subject, mapped) = await identities.SubjectOfAsync("ado", id, period, ct);
        return bot
            ? new PersonRef($"bot:{subject}", true, false)
            : new PersonRef(subject, false, mapped);
    }

    private async Task<PersonRef?> GitAuthorAsync(
        JsonElement commit,
        string period,
        CancellationToken ct
    )
    {
        if (
            !commit.TryGetProperty("author", out var author)
            || AdoPeople.Text(author, "email") is not { Length: > 0 } email
        )
            return null;
        var (subject, mapped) = await identities.SubjectOfAsync("email", email, period, ct);
        return new PersonRef(subject, false, mapped);
    }

    // The full message: lists cut a long one short and say so.
    private static async Task<string?> MessageAsync(
        AdoClient client,
        AdoRepo repo,
        JsonElement commit,
        CancellationToken ct
    )
    {
        var message = AdoPeople.Text(commit, "comment");
        if (!(commit.TryGetProperty("commentTruncated", out var cut) && cut.GetBoolean()))
            return message;
        var full = await client.GetAsync(
            $"{repo.Api}/commits/{commit.GetProperty("commitId").GetString()}",
            ct
        );
        return AdoPeople.Text(full, "comment") ?? message;
    }

    // A vote as a review state: approve (10, or 5 with suggestions), or wait and reject (-5, -10).
    private static string? VoteOf(JsonElement thread)
    {
        if (
            Property(thread, "CodeReviewThreadType") != "VoteUpdate"
            || Property(thread, "CodeReviewVoteResult") is not { } vote
        )
            return null;
        return vote switch
        {
            "10" or "5" => "APPROVED",
            "-5" or "-10" => "CHANGES_REQUESTED",
            _ => "DISMISSED",
        };
    }

    // The voter: the identity the thread's properties point at, or the author of its comment.
    private static JsonElement? VoterOf(JsonElement thread)
    {
        if (
            Property(thread, "CodeReviewVotedByIdentity") is { } key
            && thread.TryGetProperty("identities", out var ids)
            && ids.ValueKind == JsonValueKind.Object
            && ids.TryGetProperty(key, out var voter)
        )
            return voter;
        foreach (var c in thread.GetProperty("comments").EnumerateArray())
            if (c.TryGetProperty("author", out var author))
                return author;
        return null;
    }

    private static string? Property(JsonElement thread, string name) =>
        thread.TryGetProperty("properties", out var p)
        && p.ValueKind == JsonValueKind.Object
        && p.TryGetProperty(name, out var v)
            ? v.ValueKind switch
            {
                JsonValueKind.Object when v.TryGetProperty("$value", out var value) =>
                    value.ValueKind == JsonValueKind.String
                        ? value.GetString()
                        : value.GetRawText(),
                JsonValueKind.String => v.GetString(),
                _ => null,
            }
            : null;

    // Unique across the pull request: GitHub's review comment IDs are one number too.
    private static long CommentId(long thread, long comment) => thread * 10_000 + comment;

    private static int? Line(JsonElement context, string property) =>
        context.TryGetProperty(property, out var p)
        && p.ValueKind == JsonValueKind.Object
        && p.TryGetProperty("line", out var line)
        && line.ValueKind == JsonValueKind.Number
            ? line.GetInt32()
            : null;

    private static string? CommitId(JsonElement e, string property) =>
        e.TryGetProperty(property, out var c) && c.ValueKind == JsonValueKind.Object
            ? AdoPeople.Text(c, "commitId")
            : null;

    internal static DateTimeOffset? Date(JsonElement e, string property)
    {
        if (
            !e.TryGetProperty(property, out var v)
            || v.ValueKind != JsonValueKind.String
            || !v.TryGetDateTimeOffset(out var at)
        )
            return null;
        // An open pull request's closedDate is the zero date on some servers.
        return at.Year < 2000 ? null : at;
    }

    private static string Branch(string? refName) =>
        refName is null ? ""
        : refName.StartsWith("refs/heads/", StringComparison.Ordinal)
            ? refName["refs/heads/".Length..]
        : refName;
}
