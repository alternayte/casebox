using System.Net;
using System.Net.Http.Json;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json.Nodes;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Jira;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Features.WorkItems;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.Caching.Memory;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// Step 6 end to end against the GitHub and Jira contract fakes: a Jira issue, a session on its
// branch, an agent pull request with review, CI and merge, its revert, and a later fix that
// blames back to it. Nothing identifying may reach any table, the QueueBox inbox included.
public sealed class IntegrationTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private const string Alice = "alice-w";
    private const string AliceEmail = "alice.walker@example.com";
    private const string AliceName = "Alice Walker";
    private const string Bob = "bob-r";

    [Fact]
    public async Task A_work_item_collects_its_session_pull_request_review_CI_merge_revert_and_fix()
    {
        var fakes = stack.Fakes;
        var now = DateTimeOffset.UtcNow;
        var repo = fakes.Repo("acme/app");
        SeedJira(now);
        var pr7 = AgentPull(repo, now);
        SeedRevertAndFix(repo, pr7, now);

        var admin = await stack.ServerA.AdminAsync();
        await SetPromptModeAsync(admin);
        (await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = "app-work" }, Ct)).EnsureSuccessStatusCode();
        (await admin.PostAsJsonAsync("/api/v1/workspaces/app-work/repos", new { repo = "github.com/acme/app" }, Ct)).EnsureSuccessStatusCode();
        (await admin.PutAsJsonAsync("/api/v1/integrations/jira", new { url = fakes.JiraUrl.ToString(), token = FakeServices.JiraToken, projects = new[] { "PAY" } }, Ct)).EnsureSuccessStatusCode();
        (await admin.PutAsJsonAsync("/api/v1/integrations/github", new { mode = "token", token = FakeServices.GitHubToken }, Ct)).EnsureSuccessStatusCode();
        stack.ServerA.Services.GetRequiredService<IMemoryCache>().Remove($"roster:{StackFixture.OrgA}");

        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var session = $"codex:{Guid.NewGuid()}";
        (await ingest.PostAsJsonAsync("/ingest/v1/sessions", new CaptureBatch(
            new CapturedSession(session, "codex", "0.153.4", null, "github.com/acme/app", "PAY-1-fix-login", null, null, now.AddDays(-6), null, "import", null, $"⟦cbx:email:{AliceEmail}⟧"),
            [new CapturedEvent(0, now.AddDays(-6), "prompt", "fix the login", null, null, null)]), Json.Options, Ct)).EnsureSuccessStatusCode();

        await stack.ServerA.Services.GetRequiredService<JiraPoller>().PollAsync(StackFixture.OrgA, Ct);
        await Eventually(async () => (await TimelineAsync()).Contains("session_linked"));
        await stack.ServerA.Services.GetRequiredService<GitHubPoller>().PollAsync(StackFixture.OrgA, Ct);
        await Eventually(async () => (await TimelineAsync()).Contains("fix"));

        var kinds = await TimelineAsync();
        foreach (var kind in new[] { "snapshot", "session_linked", "pr_linked", "review", "ci", "merged", "revert", "fix" })
            Assert.Contains(kind, kinds);

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var link = await db.QuerySingleAsync<(string WorkItem, string Source, bool Mapped)>(
            "SELECT work_item_id, link_source, person_mapped FROM casebox.sessions WHERE org_id = @Org AND id = @Id", new { Org = StackFixture.OrgA, Id = session });
        Assert.Equal(("wi:jira:PAY-1", "branch", true), link);
        Assert.True(await db.QuerySingleAsync<bool>("SELECT is_agent FROM casebox.pull_requests WHERE org_id = @Org AND repo = 'github.com/acme/app' AND number = 7", new { Org = StackFixture.OrgA }));

        var found = await PrivacyScan.FindAsync(stack, [Alice, AliceEmail, AliceName.ToLowerInvariant(), "awalker", "jirauser1", Bob]);
        Assert.True(found.Count == 0, "Identities found in: " + string.Join(", ", found));
    }

    [Fact]
    public async Task A_signed_webhook_fetches_its_pull_request_once_and_a_forged_one_is_refused()
    {
        var fakes = stack.Fakes;
        var repo = fakes.Repo("acme/hooks");
        repo.Pulls[3] = new FakePull(3) { Title = "Add hooks", HeadRef = "hooks", AuthorLogin = "carol-h" };
        using var rsa = RSA.Create(2048);
        const string secret = "whsec_test";

        var admin = await stack.ServerB.AdminAsync();
        await SetPromptModeAsync(admin);
        (await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = "hooks" }, Ct)).EnsureSuccessStatusCode();
        (await admin.PostAsJsonAsync("/api/v1/workspaces/hooks/repos", new { repo = "github.com/acme/hooks" }, Ct)).EnsureSuccessStatusCode();
        (await admin.PutAsJsonAsync("/api/v1/integrations/github", new { mode = "app", appId = 1, installationId = 4242, privateKey = rsa.ExportRSAPrivateKeyPem(), webhookSecret = secret }, Ct)).EnsureSuccessStatusCode();

        var body = Encoding.UTF8.GetBytes("""{"action":"opened","installation":{"id":4242},"repository":{"full_name":"acme/hooks"},"pull_request":{"number":3,"user":{"login":"carol-h"}}}""");
        var delivery = Guid.NewGuid().ToString();
        var client = stack.ServerB.Anonymous();
        Assert.Equal(HttpStatusCode.Unauthorized, (await Webhook(client, body, delivery, "sha256=" + new string('0', 64))).StatusCode);
        Assert.Equal(HttpStatusCode.Accepted, (await Webhook(client, body, delivery, Sign(body, secret))).StatusCode);
        Assert.Equal(HttpStatusCode.Accepted, (await Webhook(client, body, delivery, Sign(body, secret))).StatusCode);

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await Eventually(async () => await db.QuerySingleAsync<bool>(
            "SELECT EXISTS (SELECT 1 FROM casebox.pull_requests WHERE org_id = @Org AND repo = 'github.com/acme/hooks' AND number = 3)", new { Org = StackFixture.OrgB }));
        Assert.Equal(1, await db.QuerySingleAsync<int>("SELECT count(*) FROM inbox WHERE source = 'github' AND idempotency_key = @Key", new { Key = delivery }));
        Assert.Empty(await PrivacyScan.FindAsync(stack, ["carol-h"]));
    }

    private static Task<HttpResponseMessage> Webhook(HttpClient client, byte[] body, string delivery, string signature)
    {
        var request = new HttpRequestMessage(HttpMethod.Post, "/webhooks/github") { Content = new ByteArrayContent(body) };
        request.Content.Headers.ContentType = new("application/json");
        request.Headers.Add("X-GitHub-Event", "pull_request");
        request.Headers.Add("X-GitHub-Delivery", delivery);
        request.Headers.Add("X-Hub-Signature-256", signature);
        return client.SendAsync(request, Ct);
    }

    private static string Sign(byte[] body, string secret) =>
        "sha256=" + Convert.ToHexStringLower(HMACSHA256.HashData(Encoding.UTF8.GetBytes(secret), body));

    private void SeedJira(DateTimeOffset now)
    {
        var fakes = stack.Fakes;
        var assignee = new JsonObject { ["name"] = "awalker", ["key"] = "JIRAUSER1", ["emailAddress"] = AliceEmail, ["displayName"] = AliceName };
        fakes.JiraUsers.Add((JsonObject)assignee.DeepClone());
        fakes.JiraIssues.Add(new JsonObject
        {
            ["key"] = "PAY-1",
            ["fields"] = new JsonObject
            {
                ["summary"] = $"Fix login for {AliceName}",
                ["description"] = $"Reported by {AliceEmail}, ask @{Bob}",
                ["issuetype"] = new JsonObject { ["name"] = "Bug" },
                ["labels"] = new JsonArray(),
                ["status"] = new JsonObject { ["name"] = "Done" },
                ["assignee"] = assignee,
                ["created"] = now.AddDays(-10).ToString("yyyy-MM-dd'T'HH:mm:ss.fffzzz").Remove(26, 1),
                ["updated"] = now.AddDays(-1).ToString("yyyy-MM-dd'T'HH:mm:ss.fffzzz").Remove(26, 1),
                ["resolutiondate"] = now.AddDays(-1).ToString("yyyy-MM-dd'T'HH:mm:ss.fffzzz").Remove(26, 1),
            },
        });
    }

    private static FakePull AgentPull(FakeRepo repo, DateTimeOffset now)
    {
        var pr = new FakePull(7) { Title = "PAY-1 fix login", Body = $"cc @{Bob}", HeadRef = "PAY-1-fix-login", AuthorLogin = Alice, CreatedAt = now.AddDays(-6), UpdatedAt = now.AddDays(-5), MergedAt = now.AddDays(-5) };
        pr.Commits.Add(Commit(Guid.NewGuid().ToString("N") + "abcdefgh", "fix login\n\nCo-authored-by: Claude <noreply@anthropic.com>", Alice, AliceName, AliceEmail, now.AddDays(-6)));
        pr.Reviews.Add(new JsonObject { ["id"] = 1, ["state"] = "CHANGES_REQUESTED", ["user"] = User(Bob), ["submitted_at"] = now.AddDays(-5.5).ToString("O"), ["body"] = $"@{Alice} use the real db" });
        pr.Comments.Add(new JsonObject { ["id"] = 11, ["pull_request_review_id"] = 1, ["path"] = "src/login.go", ["line"] = 10, ["original_line"] = 10, ["commit_id"] = pr.HeadSha, ["user"] = User(Bob), ["created_at"] = now.AddDays(-5.5).ToString("O"), ["body"] = "no mocks" });
        pr.Checks.Add(new JsonObject { ["name"] = "test", ["conclusion"] = "success", ["completed_at"] = now.AddDays(-5.2).ToString("O") });
        pr.Files.Add(new JsonObject { ["filename"] = "src/login.go", ["additions"] = 1, ["deletions"] = 1, ["patch"] = "@@ -8,3 +8,3 @@\n ctx\n-old\n+new\n end" });
        repo.Pulls[7] = pr;
        return pr;
    }

    private static void SeedRevertAndFix(FakeRepo repo, FakePull pr7, DateTimeOffset now)
    {
        repo.Pulls[9] = new FakePull(9) { Title = "Revert \"PAY-1 fix login\"", Body = "Reverts acme/app#7", HeadRef = "revert-7", AuthorLogin = Bob, CreatedAt = now.AddDays(-4.5), UpdatedAt = now.AddDays(-4), MergedAt = now.AddDays(-4) };
        var fix = new FakePull(11) { Title = "Handle an empty password", HeadRef = "empty-password", AuthorLogin = Bob, CreatedAt = now.AddDays(-3.5), UpdatedAt = now.AddDays(-3), MergedAt = now.AddDays(-3) };
        fix.Files.Add(new JsonObject { ["filename"] = "src/login.go", ["additions"] = 1, ["deletions"] = 1, ["patch"] = "@@ -9,2 +9,2 @@\n-new\n+newer\n end" });
        repo.Pulls[11] = fix;
        repo.Blame[$"{fix.BaseSha}:src/login.go"] = [new BlameRange(1, 8, 2), new BlameRange(9, 9, 7), new BlameRange(10, 20, 2)];
        _ = pr7;
    }

    private static JsonObject Commit(string sha, string message, string login, string name, string email, DateTimeOffset at) => new()
    {
        ["sha"] = sha,
        ["author"] = User(login),
        ["commit"] = new JsonObject
        {
            ["message"] = message,
            ["author"] = new JsonObject { ["name"] = name, ["email"] = email, ["date"] = at.ToString("O") },
            ["committer"] = new JsonObject { ["name"] = name, ["email"] = email, ["date"] = at.ToString("O") },
        },
    };

    private static JsonObject User(string login) => new() { ["login"] = login, ["type"] = "User" };

    private async Task<List<string>> TimelineAsync()
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        return (await db.QueryAsync<string>("SELECT kind FROM casebox.work_item_timeline WHERE org_id = @Org AND work_item_id = 'wi:jira:PAY-1'", new { Org = StackFixture.OrgA })).ToList();
    }

    private static async Task SetPromptModeAsync(HttpClient admin)
    {
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>("/api/v1/org", Json.Options, Ct);
        if (org!.Settings.PromptMode is null)
            (await admin.PutAsJsonAsync("/api/v1/org/settings", org.Settings with { PromptMode = PromptMode.Redacted }, Json.Options, Ct)).EnsureSuccessStatusCode();
    }

    private static async Task Eventually(Func<Task<bool>> probe)
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (!await probe())
        {
            Assert.True(DateTime.UtcNow < deadline, "The condition did not hold within 60 seconds.");
            await Task.Delay(300, Ct);
        }
    }
}
