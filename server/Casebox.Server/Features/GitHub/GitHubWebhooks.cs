using System.Data.Common;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Integrations;
using Dapper;
using Deedbox;
using Microsoft.Extensions.Options;
using Npgsql;
using QueueBox.Inbox;

namespace Casebox.Server.Features.GitHub;

// What a webhook changed, and nothing else: no login, name, email or text leaves the request.
public sealed record GitHubNotice(string Org, string Event, string Repo, IReadOnlyList<int> PullRequests, int? Issue);

// GitHub App webhooks (gate 1, decision 1): the server checks the HMAC, reduces the delivery to a
// notice that holds no identity, and forwards it to the QueueBox github source, keyed on
// X-GitHub-Delivery. The notice's handler fetches the changed objects through the poll path.
public static class GitHubWebhooks
{
    public const string Source = "github";

    private static readonly HashSet<string> Events =
        ["pull_request", "pull_request_review", "pull_request_review_comment", "check_run", "workflow_run", "push", "issues"];

    public static void MapGitHubWebhooks(this IEndpointRouteBuilder app)
    {
        app.MapPost("/webhooks/github", async (HttpContext http, NpgsqlDataSource db, IntegrationStore integrations, IHttpClientFactory httpFactory, IOptions<CaseboxOptions> options) =>
        {
            using var buffer = new MemoryStream();
            await http.Request.Body.CopyToAsync(buffer, http.RequestAborted);
            var body = buffer.ToArray();
            var delivery = http.Request.Headers["X-GitHub-Delivery"].ToString();
            var kind = http.Request.Headers["X-GitHub-Event"].ToString();
            if (string.IsNullOrEmpty(delivery) || string.IsNullOrEmpty(kind)) return Results.BadRequest();

            using var document = JsonDocument.Parse(body);
            var root = document.RootElement;
            if (!root.TryGetProperty("installation", out var installation) || !installation.TryGetProperty("id", out var installationId))
                return Results.Unauthorized();

            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var org = await connection.QuerySingleOrDefaultAsync<string>(new CommandDefinition(
                "SELECT org_id FROM casebox.integrations WHERE kind = 'github' AND config->>'mode' = 'app' AND (config->>'installationId')::bigint = @Id",
                new { Id = installationId.GetInt64() }, cancellationToken: http.RequestAborted));
            if (org is null) return Results.Unauthorized();
            var secret = (await integrations.GetAsync<GitHubSettings, GitHubSecret>(org, "github", http.RequestAborted))!.Value.Secret.WebhookSecret!;
            if (!SignatureValid(body, secret, http.Request.Headers["X-Hub-Signature-256"].ToString())) return Results.Unauthorized();

            if (!Events.Contains(kind) || Notice(org, kind, root) is not { } notice) return Results.Accepted();

            using var forward = new HttpRequestMessage(HttpMethod.Post, new Uri(options.Value.QueueBox.BaseUrl, "/inbox/github"))
            {
                Content = JsonContent(notice),
            };
            forward.Headers.Authorization = new AuthenticationHeaderValue("Bearer", options.Value.QueueBox.PollToken);
            forward.Headers.Add("X-GitHub-Delivery", delivery);
            forward.Headers.Add("X-GitHub-Event", kind);
            using var response = await httpFactory.CreateClient("queuebox").SendAsync(forward, http.RequestAborted);
            return response.IsSuccessStatusCode ? Results.Accepted() : Results.StatusCode(StatusCodes.Status502BadGateway);
        }).AllowAnonymous().ExcludeFromDescription();
    }

    public static bool SignatureValid(byte[] body, string secret, string header)
    {
        if (!header.StartsWith("sha256=", StringComparison.Ordinal)) return false;
        var expected = Encoding.ASCII.GetBytes("sha256=" + Convert.ToHexStringLower(HMACSHA256.HashData(Encoding.UTF8.GetBytes(secret), body)));
        return CryptographicOperations.FixedTimeEquals(expected, Encoding.ASCII.GetBytes(header));
    }

    private static GitHubNotice? Notice(string org, string kind, JsonElement root)
    {
        if (!root.TryGetProperty("repository", out var repository)) return null;
        var repo = $"github.com/{repository.GetProperty("full_name").GetString()}".ToLowerInvariant();
        var pulls = new List<int>();
        int? issue = null;
        switch (kind)
        {
            case "pull_request" or "pull_request_review" or "pull_request_review_comment":
                pulls.Add(root.GetProperty("pull_request").GetProperty("number").GetInt32());
                break;
            case "check_run" or "workflow_run":
                var run = root.GetProperty(kind);
                if (run.TryGetProperty("pull_requests", out var prs))
                    pulls.AddRange(prs.EnumerateArray().Select(p => p.GetProperty("number").GetInt32()));
                if (pulls.Count == 0) return null;
                break;
            case "issues":
                issue = root.GetProperty("issue").GetProperty("number").GetInt32();
                break;
        }

        return new GitHubNotice(org, kind, repo, pulls, issue);
    }

    private static StringContent JsonContent(GitHubNotice notice) =>
        new(JsonSerializer.Serialize(notice, GitHubJson.Options), Encoding.UTF8, "application/json");
}

// Handles one notice: fetches what changed through the poll path, which tokenizes it.
public sealed class GitHubNoticeHandler(GitHubPoller poller) : IInboxHandler
{
    public string Source => GitHubWebhooks.Source;

    public string EventType { get; init; } = "";

    public async Task HandleAsync(InboxMessage message, IEventStore store, DbTransaction transaction, CancellationToken ct)
    {
        var notice = message.Payload.Deserialize<GitHubNotice>(GitHubJson.Options)!;
        foreach (var number in notice.PullRequests)
            await poller.RefreshPullAsync(notice.Org, notice.Repo, number, ct);
        if (notice.Issue is { } issue)
            await poller.RefreshIssueAsync(notice.Org, notice.Repo, issue, ct);
        if (notice.Event == "push")
            await poller.PollAsync(notice.Org, ct);
    }
}
