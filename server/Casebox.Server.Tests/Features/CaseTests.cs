using System.Net;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.Caching.Memory;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// Step 9 on the server: a merged pull request becomes a candidate, a worker's mine, validate and
// instruction answers move the case through its stream, and a person reviews it.
[Collection(GitHubPolling.Name)]
public sealed class CaseTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task A_merged_pull_request_becomes_a_reviewed_case_through_the_workers_answers()
    {
        var tag = Guid.NewGuid().ToString("N")[..8];
        var name = $"acme/cases-{tag}";
        var repo = $"github.com/{name}";
        var now = DateTimeOffset.UtcNow;
        var pr = new FakePull(31)
        {
            Title = "Add retries",
            HeadRef = "retries",
            AuthorLogin = "grace-team",
            CreatedAt = now.AddDays(-4),
            UpdatedAt = now.AddDays(-3),
            MergedAt = now.AddDays(-3),
        };
        pr.Commits.Add(
            new JsonObject
            {
                ["sha"] = FakePull.Sha(),
                ["author"] = new JsonObject { ["login"] = "grace-team", ["type"] = "User" },
                ["commit"] = new JsonObject
                {
                    ["message"] = "add retries",
                    ["author"] = new JsonObject
                    {
                        ["name"] = "g",
                        ["email"] = "g@users.noreply.example.com",
                        ["date"] = now.AddDays(-4).ToString("O"),
                    },
                    ["committer"] = new JsonObject
                    {
                        ["name"] = "g",
                        ["email"] = "g@users.noreply.example.com",
                        ["date"] = now.AddDays(-4).ToString("O"),
                    },
                },
            }
        );
        stack.Fakes.Repo(name).Pulls[31] = pr;

        var admin = await stack.ServerA.AdminAsync();
        var workspace = $"cases-{tag}";
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync($"/api/v1/workspaces/{workspace}/repos", new { repo }, Ct)
        ).EnsureSuccessStatusCode();

        // No mining before the recipe is confirmed.
        Assert.Equal(
            HttpStatusCode.UnprocessableEntity,
            (await admin.PostAsync($"/api/v1/workspaces/{workspace}/mining", null, Ct)).StatusCode
        );
        var recipe = JsonDocument
            .Parse(
                """{"image":"golang:1.26","install":[],"lockfiles":["go.mod"],"test":[{"command":"go test ./..."}],"services":{},"links":[]}"""
            )
            .RootElement;
        var hash = (
            await (
                await admin.PutAsJsonAsync(
                    $"/api/v1/workspaces/{workspace}/recipe",
                    new { recipe },
                    Ct
                )
            ).Content.ReadFromJsonAsync<JsonElement>(Ct)
        )
            .GetProperty("recipeHash")
            .GetString();
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/recipe/validation",
                new { hash, passed = true },
                Ct
            )
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/recipe/confirmation",
                new { hash },
                Ct
            )
        ).EnsureSuccessStatusCode();

        (
            await admin.PutAsJsonAsync(
                "/api/v1/integrations/github",
                new { mode = "token", token = FakeServices.GitHubToken },
                Ct
            )
        ).EnsureSuccessStatusCode();
        stack
            .ServerA.Services.GetRequiredService<IMemoryCache>()
            .Remove($"roster:{StackFixture.OrgA}");
        await stack
            .ServerA.Services.GetRequiredService<GitHubPoller>()
            .PollAsync(StackFixture.OrgA, Ct);
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        await using (var db = new NpgsqlConnection(stack.ConnectionString))
            await Eventually(async () =>
                await db.QuerySingleAsync<bool>(
                    "SELECT EXISTS (SELECT 1 FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = 31)",
                    new { Org = StackFixture.OrgA, Repo = repo }
                )
            );
        (
            await admin.PostAsync($"/api/v1/workspaces/{workspace}/mining", null, Ct)
        ).EnsureSuccessStatusCode();

        var mine = await LeaseAsync(
            worker,
            "mine",
            j =>
                j["payload"]!["repo"]!.GetValue<string>() == repo
                && j["payload"]!["candidates"]!.AsArray().Count > 0
        );
        var candidate = mine["payload"]!["candidates"]!
            .AsArray()
            .Single(c => c!["key"]!.GetValue<string>() == $"pr:{repo}#31")!;
        Assert.Equal("capability", candidate["kind"]!.GetValue<string>());
        var pull = candidate["pulls"]![0]!;
        await CompleteAsync(
            worker,
            mine,
            new
            {
                cases = new[]
                {
                    new
                    {
                        key = $"pr:{repo}#31",
                        kind = "capability",
                        scope = "single",
                        harnessHash = new string('b', 64),
                        rank = 5,
                        repos = new[]
                        {
                            new
                            {
                                repo,
                                @base = pull["baseSha"]!.GetValue<string>(),
                                merged = pull["mergeSha"]!.GetValue<string>(),
                                role = "sealed",
                            },
                        },
                    },
                },
            }
        );

        var validate = await LeaseAsync(
            worker,
            "case.validate",
            j => j["payload"]!["repos"]![0]!["repo"]!.GetValue<string>() == repo
        );
        var caseId = validate["payload"]!["caseId"]!.GetValue<string>();
        Assert.Equal("golang:1.26", validate["payload"]!["recipe"]!["image"]!.GetValue<string>());
        await CompleteAsync(
            worker,
            validate,
            new
            {
                passed = true,
                oracle = new string('c', 64),
                failToPass = 2,
                passToPass = 11,
                seconds = 40.5,
                drift = true,
                weight = 0.5,
                retire = false,
            }
        );

        var instruction = await LeaseAsync(
            worker,
            "case.instruction",
            j => j["payload"]!["caseId"]!.GetValue<string>() == caseId
        );
        var source = await worker.GetFromJsonAsync<JsonElement>(
            $"/worker/v1/cases/{caseId}/source",
            Ct
        );
        Assert.Equal("Add retries", source.GetProperty("pullTitle").GetString());
        await CompleteAsync(
            worker,
            instruction,
            new
            {
                text = $"Retry failed payments, as {TestRoster.TeamEmail("grace")} asked.",
                signatures = new[] { "func Retry(n int) error" },
                model = "test-model",
            }
        );

        // The instruction leaves the API masked; the person's email became a token on the way in.
        var queue = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/cases/queue?workspace={workspace}",
            Ct
        );
        var queued = Assert.Single(queue.EnumerateArray());
        Assert.Equal(
            "Retry failed payments, as [person] asked.",
            queued.GetProperty("instruction").GetString()
        );
        Assert.Equal(
            (2, 0.5),
            (
                queued.GetProperty("summary").GetProperty("failToPass").GetInt32(),
                queued.GetProperty("summary").GetProperty("weight").GetDouble()
            )
        );

        (
            await admin.PutAsJsonAsync(
                $"/api/v1/cases/{caseId}/instruction",
                new { text = "Retry failed payments up to three times." },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var bulk = await (
            await admin.PostAsJsonAsync(
                "/api/v1/cases/approvals",
                new { ids = new[] { caseId, "0000000000000000000a" } },
                Ct
            )
        ).Content.ReadFromJsonAsync<JsonElement>(Ct);
        Assert.Equal(
            [caseId],
            bulk.GetProperty("approved").EnumerateArray().Select(e => e.GetString())
        );
        Assert.Equal(1, bulk.GetProperty("refused").GetArrayLength());

        var approved = await admin.GetFromJsonAsync<JsonElement>($"/api/v1/cases/{caseId}", Ct);
        Assert.Equal(
            ("approved", "dev"),
            (
                approved.GetProperty("summary").GetProperty("status").GetString(),
                approved.GetProperty("summary").GetProperty("split").GetString()
            )
        );
        Assert.Equal(
            "Retry failed payments up to three times.",
            approved.GetProperty("instruction").GetString()
        );

        // Mining again finds the same source and appends nothing.
        (
            await admin.PostAsync($"/api/v1/workspaces/{workspace}/mining", null, Ct)
        ).EnsureSuccessStatusCode();
        var again = await LeaseAsync(
            worker,
            "mine",
            j => j["payload"]!["repo"]!.GetValue<string>() == repo
        );
        await CompleteAsync(
            worker,
            again,
            new
            {
                cases = new[]
                {
                    new
                    {
                        key = $"pr:{repo}#31",
                        kind = "capability",
                        scope = "single",
                        harnessHash = new string('b', 64),
                        rank = 5,
                        repos = new[]
                        {
                            new
                            {
                                repo,
                                @base = "x",
                                merged = "y",
                                role = "sealed",
                            },
                        },
                    },
                },
            }
        );
        var all = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/cases?workspace={workspace}",
            Ct
        );
        Assert.Equal(1, all.GetArrayLength());
        Assert.Empty(
            await PrivacyScan.FindAsync(stack, ["grace-team", TestRoster.TeamEmail("grace")])
        );
    }

    private static async Task<JsonObject> LeaseAsync(
        HttpClient worker,
        string kind,
        Func<JsonObject, bool> mine
    )
    {
        for (var i = 0; i < 100; i++)
        {
            var lease = await worker.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "case-test",
                    version = "test",
                    kinds = new[] { kind },
                },
                Ct
            );
            if (lease.StatusCode == HttpStatusCode.NoContent)
            {
                await Task.Delay(300, Ct);
                continue;
            }

            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            if (mine(job))
                return job;
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/fail",
                    new
                    {
                        workerId = "case-test",
                        error = "not this test's job",
                        retryable = false,
                    },
                    Ct
                )
            ).EnsureSuccessStatusCode();
        }

        throw new InvalidOperationException($"No {kind} job for this test.");
    }

    private static async Task CompleteAsync(HttpClient worker, JsonObject job, object result) =>
        (
            await worker.PostAsJsonAsync(
                $"/worker/v1/jobs/{job["id"]}/complete",
                new { workerId = "case-test", result },
                Ct
            )
        ).EnsureSuccessStatusCode();

    private static async Task Eventually(Func<Task<bool>> probe)
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (!await probe())
        {
            Assert.True(
                DateTime.UtcNow < deadline,
                "The condition did not hold within 60 seconds."
            );
            await Task.Delay(300, Ct);
        }
    }
}
