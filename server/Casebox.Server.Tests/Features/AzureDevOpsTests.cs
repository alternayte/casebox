using System.Net;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Features.AzureDevOps;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.Caching.Memory;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// Step 15 against the Azure DevOps Server contract fake: a completed agent pull request with a
// review thread a later commit answered, a failed then passing status, a build policy, a vote, a
// build service, and a revert pushed to main. The steering signals come out as they do for GitHub,
// the people map through _apis/identities, and no identity ID, account, mail or display name
// reaches any table.
[Collection(GitHubPolling.Name)]
public sealed class AzureDevOpsTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private static readonly JsonObject Grace = FakeAzureDevOps.Person(
        "0b9c1a2e-1111-4a5b-9c0d-000000000001",
        "Grace Team",
        TestRoster.TeamEmail("grace"),
        @"CONTOSO\ghopper"
    );
    private static readonly JsonObject Margaret = FakeAzureDevOps.Person(
        "0b9c1a2e-2222-4a5b-9c0d-000000000002",
        "Margaret Team",
        TestRoster.TeamEmail("margaret"),
        @"CONTOSO\mhamilton"
    );
    private static readonly JsonObject Edsger = FakeAzureDevOps.Person(
        "0b9c1a2e-3333-4a5b-9c0d-000000000003",
        "Edsger Team",
        TestRoster.TeamEmail("edsger"),
        @"CONTOSO\edijkstra"
    );

    // An identity without mail: a bot, whatever it says.
    private static readonly JsonObject NoMail = FakeAzureDevOps.Person(
        "0b9c1a2e-4444-4a5b-9c0d-000000000004",
        "Kiosk Account",
        null,
        @"CONTOSO\kiosk01"
    );
    private static readonly JsonObject BuildService = FakeAzureDevOps.Person(
        "0b9c1a2e-5555-4a5b-9c0d-000000000005",
        "Payments Build Service (DefaultCollection)",
        null,
        @"Build\0b9c1a2e-5555"
    );

    [Fact]
    public async Task An_Azure_DevOps_pull_request_gives_the_same_steering_signals_as_GitHub()
    {
        var ado = stack.Fakes.Ado;
        foreach (var person in new[] { Grace, Margaret, Edsger, NoMail })
            ado.AddIdentity(person);
        var tag = Guid.NewGuid().ToString("N")[..8];
        var fake = ado.Repo("Payments", $"payments-{tag}");
        var repo =
            $"{stack.Fakes.AdoCollection["http://".Length..].Replace($":{stack.Fakes.Port}", "", StringComparison.Ordinal)}/payments/payments-{tag}".ToLowerInvariant();
        var now = DateTimeOffset.UtcNow;
        var agentSha = FakePull.Sha();
        var fixSha = FakePull.Sha();
        var mergeSha = FakePull.Sha();
        var revertSha = FakePull.Sha();

        var pull = new FakeAdoPull(
            new JsonObject
            {
                ["pullRequestId"] = 31,
                ["status"] = "completed",
                ["title"] = "Add export",
                ["description"] =
                    @"Asked by @<0B9C1A2E-3333-4A5B-9C0D-000000000003> and CONTOSO\edijkstra",
                ["createdBy"] = FakeAzureDevOps.Ref(Grace),
                ["creationDate"] = now.AddDays(-6).ToString("O"),
                ["closedDate"] = now.AddDays(-5).ToString("O"),
                ["sourceRefName"] = "refs/heads/export",
                ["targetRefName"] = "refs/heads/main",
                ["isDraft"] = false,
                ["lastMergeSourceCommit"] = new JsonObject { ["commitId"] = fixSha },
                ["lastMergeTargetCommit"] = new JsonObject { ["commitId"] = FakePull.Sha() },
                ["lastMergeCommit"] = new JsonObject { ["commitId"] = FakePull.Sha() },
                ["reviewers"] = new JsonArray(
                    FakeAzureDevOps.Ref(Margaret),
                    FakeAzureDevOps.Ref(Edsger),
                    FakeAzureDevOps.Ref(BuildService)
                ),
                ["labels"] = new JsonArray(
                    new JsonObject { ["name"] = "export", ["active"] = true }
                ),
            }
        );
        pull.Iterations.Add(Iteration(1, agentSha, now.AddDays(-6)));
        pull.Iterations.Add(Iteration(2, fixSha, now.AddDays(-5.8)));
        pull.Files.Add("src/export.go");
        pull.Commits.Add(
            Commit(
                agentSha,
                "add export\n\nCo-authored-by: Claude <noreply@anthropic.com>",
                TestRoster.TeamEmail("grace"),
                now.AddDays(-6)
            )
        );
        pull.Commits.Add(
            Commit(
                fixSha,
                "fix the flaky assertion",
                TestRoster.TeamEmail("linus"),
                now.AddDays(-5.8)
            )
        );
        pull.Threads.Add(
            new JsonObject
            {
                ["id"] = 7,
                ["publishedDate"] = now.AddDays(-5.9).ToString("O"),
                ["lastUpdatedDate"] = now.AddDays(-5.9).ToString("O"),
                ["status"] = "fixed",
                ["threadContext"] = new JsonObject
                {
                    ["filePath"] = "/src/export.go",
                    ["rightFileStart"] = new JsonObject { ["line"] = 10, ["offset"] = 1 },
                    ["rightFileEnd"] = new JsonObject { ["line"] = 10, ["offset"] = 20 },
                },
                ["pullRequestThreadContext"] = new JsonObject
                {
                    ["iterationContext"] = new JsonObject
                    {
                        ["firstComparingIteration"] = 1,
                        ["secondComparingIteration"] = 1,
                    },
                },
                ["comments"] = new JsonArray(
                    Comment(
                        1,
                        0,
                        Margaret,
                        "please don't swallow the error here",
                        now.AddDays(-5.9)
                    ),
                    Comment(2, 1, Grace, "done", now.AddDays(-5.85))
                ),
            }
        );
        pull.Threads.Add(
            new JsonObject
            {
                ["id"] = 8,
                ["publishedDate"] = now.AddDays(-5.5).ToString("O"),
                ["lastUpdatedDate"] = now.AddDays(-5.5).ToString("O"),
                ["properties"] = new JsonObject
                {
                    ["CodeReviewThreadType"] = new JsonObject
                    {
                        ["$type"] = "System.String",
                        ["$value"] = "VoteUpdate",
                    },
                    ["CodeReviewVoteResult"] = new JsonObject
                    {
                        ["$type"] = "System.Int32",
                        ["$value"] = 10,
                    },
                },
                ["comments"] = new JsonArray(
                    new JsonObject
                    {
                        ["id"] = 1,
                        ["parentCommentId"] = 0,
                        ["author"] = FakeAzureDevOps.Ref(Edsger),
                        ["content"] = "Edsger Team voted 10",
                        ["commentType"] = "system",
                        ["publishedDate"] = now.AddDays(-5.5).ToString("O"),
                    }
                ),
            }
        );
        pull.Threads.Add(
            new JsonObject
            {
                ["id"] = 9,
                ["publishedDate"] = now.AddDays(-5.95).ToString("O"),
                ["lastUpdatedDate"] = now.AddDays(-5.95).ToString("O"),
                ["threadContext"] = new JsonObject
                {
                    ["filePath"] = "/src/export.go",
                    ["rightFileStart"] = new JsonObject { ["line"] = 3, ["offset"] = 1 },
                },
                ["comments"] = new JsonArray(
                    Comment(1, 0, NoMail, "kiosk says hi", now.AddDays(-5.95))
                ),
            }
        );
        pull.Statuses.Add(Status(1, "failed", "ci", "test", now.AddDays(-5.9)));
        pull.Statuses.Add(Status(2, "succeeded", "ci", "test", now.AddDays(-5.7)));
        pull.Evaluations.Add(
            new JsonObject
            {
                ["evaluationId"] = Guid.NewGuid().ToString(),
                ["status"] = "approved",
                ["startedDate"] = now.AddDays(-5.75).ToString("O"),
                ["completedDate"] = now.AddDays(-5.7).ToString("O"),
                ["configuration"] = new JsonObject
                {
                    ["type"] = new JsonObject
                    {
                        ["id"] = "0609b952-1397-4640-95ec-e00a01b2c241",
                        ["displayName"] = "Build",
                    },
                    ["settings"] = new JsonObject { ["displayName"] = "payments-ci" },
                },
            }
        );
        fake.Pulls[31] = pull;
        fake.DefaultBranchCommits.Add(
            Commit(
                mergeSha,
                "Merged PR 31: Add export",
                TestRoster.TeamEmail("grace"),
                now.AddDays(-5)
            )
        );
        fake.DefaultBranchCommits.Add(
            Commit(
                revertSha,
                $"Revert \"Merged PR 31: Add export\"\n\nThe export breaks the nightly job.\n\nThis reverts commit {mergeSha}.",
                TestRoster.TeamEmail("edsger"),
                now.AddDays(-4)
            )
        );

        var admin = await stack.ServerA.AdminAsync();
        await SetPromptModeAsync(admin);
        var workspace = $"ado-{tag}";
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();

        // A wrong token is refused before it is stored.
        var refused = await admin.PutAsJsonAsync(
            "/api/v1/integrations/azure-devops",
            new { url = stack.Fakes.AdoCollection, token = "wrong" },
            Ct
        );
        Assert.Equal(HttpStatusCode.UnprocessableEntity, refused.StatusCode);
        Assert.Contains(
            "CBX073",
            await refused.Content.ReadAsStringAsync(Ct),
            StringComparison.Ordinal
        );

        (
            await admin.PutAsJsonAsync(
                "/api/v1/integrations/azure-devops",
                new { url = stack.Fakes.AdoCollection + "/", token = FakeAzureDevOps.Token },
                Ct
            )
        ).EnsureSuccessStatusCode();
        // The remote as git shows it: the collection's prefix finds the code host.
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/repos",
                new
                {
                    repo = $"http://{repo.Replace("/payments/", "/Payments/_git/", StringComparison.Ordinal)}",
                },
                Ct
            )
        ).EnsureSuccessStatusCode();
        stack
            .ServerA.Services.GetRequiredService<IMemoryCache>()
            .Remove($"roster:{StackFixture.OrgA}");

        var status = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/repos/host?repo={Uri.EscapeDataString(repo)}",
            Ct
        );
        Assert.Equal(
            ("azure-devops", true, true, true),
            (
                status.GetProperty("host").GetString(),
                status.GetProperty("connected").GetBoolean(),
                status.GetProperty("reachable").GetBoolean(),
                status.GetProperty("identitiesReadable").GetBoolean()
            )
        );

        var result = await stack
            .ServerA.Services.GetRequiredService<AdoPoller>()
            .PollAsync(StackFixture.OrgA, Ct);
        Assert.Null(result.Error);
        Assert.Equal(1, result.Reverts);

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await Eventually(async () =>
            await db.QuerySingleAsync<int>(
                "SELECT count(*) FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = 31",
                new { Org = StackFixture.OrgA, Repo = repo }
            ) == 1
        );
        var row = await db.QuerySingleAsync<(string State, string? MergeSha, bool Agent)>(
            "SELECT state, merge_sha, is_agent FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = 31",
            new { Org = StackFixture.OrgA, Repo = repo }
        );
        Assert.Equal(("merged", mergeSha, true), (row.State, row.MergeSha, row.Agent));

        await RefreshAsync(admin, repo);
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        var repos = await worker.GetFromJsonAsync<JsonElement>("/worker/v1/repos", Ct);
        Assert.Contains(
            repos.EnumerateArray(),
            r =>
                r.GetProperty("repo").GetString() == repo
                && r.GetProperty("host").GetString() == "azure-devops"
                && r.GetProperty("cloneUrl").GetString()
                    == $"{stack.Fakes.AdoCollection}/payments/_git/payments-{tag}"
        );

        var job = await LeaseAsync(
            worker,
            "steering.pr",
            j => j["payload"]!["repo"]!.GetValue<string>() == repo
        );
        var payload = job["payload"]!;
        Assert.Equal(
            [true, false],
            payload["commits"]!.AsArray().Select(c => c!["agent"]!.GetValue<bool>())
        );
        // The kiosk's comment is a bot's and the reply is no root: one comment to check.
        var comment = Assert.Single(payload["comments"]!.AsArray());
        Assert.Equal(
            (70_001L, 10, agentSha),
            (
                comment!["id"]!.GetValue<long>(),
                comment["line"]!.GetValue<int>(),
                comment["commitSha"]!.GetValue<string>()
            )
        );
        (
            await worker.PostAsJsonAsync(
                $"/worker/v1/jobs/{job["id"]}/complete",
                new
                {
                    workerId = "ado-test",
                    result = new
                    {
                        rewrites = new[]
                        {
                            new
                            {
                                sha = fixSha,
                                lines = 3,
                                files = new[] { "src/export.go" },
                            },
                        },
                        reviewChanges = new[] { new { commentId = 70_001L, sha = fixSha } },
                    },
                },
                Ct
            )
        ).EnsureSuccessStatusCode();

        var facts = (
            await db.QueryAsync<(string Signal, string Phase, bool Mapped, string? Text)>(
                "SELECT signal, phase, person_mapped, text FROM casebox.steering_facts WHERE org_id = @Org AND repo = @Repo",
                new { Org = StackFixture.OrgA, Repo = repo }
            )
        ).ToList();
        Assert.Equal(
            [
                ("ci_fix", "before_merge", true),
                ("human_rewrite", "before_merge", true),
                ("revert", "after_merge", true),
                ("review_change", "before_merge", true),
            ],
            facts
                .OrderBy(f => f.Signal, StringComparer.Ordinal)
                .Select(f => (f.Signal, f.Phase, f.Mapped))
                .ToList()
        );
        Assert.Contains(
            "swallow the error",
            facts.Single(f => f.Signal == "review_change").Text,
            StringComparison.Ordinal
        );
        Assert.Contains(
            "nightly job",
            facts.Single(f => f.Signal == "revert").Text,
            StringComparison.Ordinal
        );

        // The reviewers are three people for k, the build service none; the kiosk is a bot.
        var snapshot = await db.QuerySingleAsync<string>(
            "SELECT snapshot::text FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = 31",
            new { Org = StackFixture.OrgA, Repo = repo }
        );
        var pr = JsonDocument.Parse(snapshot).RootElement;
        Assert.Equal(
            [("APPROVED", false)],
            pr.GetProperty("reviews")
                .EnumerateArray()
                .Select(r =>
                    (
                        r.GetProperty("state").GetString(),
                        r.GetProperty("author").GetProperty("bot").GetBoolean()
                    )
                )
        );
        Assert.Contains(
            pr.GetProperty("reviewComments").EnumerateArray(),
            c => c.GetProperty("author").GetProperty("bot").GetBoolean()
        );
        Assert.Equal(
            ["build/payments-ci", "ci/test", "ci/test"],
            pr.GetProperty("checks")
                .EnumerateArray()
                .Select(c => c.GetProperty("name").GetString())
                .Order(StringComparer.Ordinal)
        );

        // LIKE reads a backslash as an escape, so the accounts are looked for by their parts.
        var needles = new List<string>
        {
            "contoso",
            "ghopper",
            "mhamilton",
            "edijkstra",
            "kiosk01",
        };
        foreach (var person in new[] { Grace, Margaret, Edsger, NoMail, BuildService })
        {
            needles.Add(((string)person["id"]!).ToLowerInvariant());
            needles.Add(((string)person["displayName"]!).ToLowerInvariant());
            if ((string?)person["_mail"] is { } mail)
                needles.Add(mail);
        }
        var found = await PrivacyScan.FindAsync(stack, [.. needles]);
        Assert.True(found.Count == 0, "Identities found in: " + string.Join(", ", found));
    }

    [Fact]
    public async Task A_repository_on_an_unknown_collection_is_refused_until_its_collection_is_named()
    {
        var admin = await stack.ServerB.AdminAsync();
        await SetPromptModeAsync(admin);
        var tag = Guid.NewGuid().ToString("N")[..8];
        var workspace = $"ado-host-{tag}";
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();
        var remote = $"https://ado.example.com/tfs/DefaultCollection/Payments/_git/api-{tag}";

        var refused = await admin.PostAsJsonAsync(
            $"/api/v1/workspaces/{workspace}/repos",
            new { repo = remote },
            Ct
        );
        Assert.Equal(HttpStatusCode.UnprocessableEntity, refused.StatusCode);
        Assert.Contains(
            "CBX041",
            await refused.Content.ReadAsStringAsync(Ct),
            StringComparison.Ordinal
        );

        var elsewhere = await admin.PostAsJsonAsync(
            $"/api/v1/workspaces/{workspace}/repos",
            new { repo = remote, collection = "https://other.example.com/tfs/DefaultCollection" },
            Ct
        );
        Assert.Equal(HttpStatusCode.UnprocessableEntity, elsewhere.StatusCode);

        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/repos",
                new
                {
                    repo = remote,
                    collection = "https://ado.example.com:8443/tfs/DefaultCollection/",
                },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var repo = $"ado.example.com/tfs/defaultcollection/payments/api-{tag}";
        var status = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/repos/host?repo={Uri.EscapeDataString(repo)}",
            Ct
        );
        Assert.Equal(
            ("azure-devops", "https://ado.example.com:8443/tfs/DefaultCollection", false),
            (
                status.GetProperty("host").GetString(),
                status.GetProperty("collection").GetString(),
                status.GetProperty("connected").GetBoolean()
            )
        );

        var worker = await stack.ServerB.TokenClientAsync(TokenKind.Worker);
        var repos = await worker.GetFromJsonAsync<JsonElement>("/worker/v1/repos", Ct);
        Assert.Contains(
            repos.EnumerateArray(),
            r =>
                r.GetProperty("repo").GetString() == repo
                && r.GetProperty("cloneUrl").GetString()
                    == $"https://ado.example.com:8443/tfs/DefaultCollection/payments/_git/api-{tag}"
        );
    }

    private static JsonObject Iteration(int id, string sha, DateTimeOffset at) =>
        new()
        {
            ["id"] = id,
            ["sourceRefCommit"] = new JsonObject { ["commitId"] = sha },
            ["createdDate"] = at.ToString("O"),
            ["updatedDate"] = at.ToString("O"),
        };

    private static JsonObject Commit(string sha, string message, string email, DateTimeOffset at) =>
        new()
        {
            ["commitId"] = sha,
            ["comment"] = message,
            ["author"] = new JsonObject
            {
                ["name"] = email.Split('@')[0],
                ["email"] = email,
                ["date"] = at.ToString("O"),
            },
            ["committer"] = new JsonObject
            {
                ["name"] = email.Split('@')[0],
                ["email"] = email,
                ["date"] = at.ToString("O"),
            },
        };

    private static JsonObject Comment(
        int id,
        int parent,
        JsonObject author,
        string content,
        DateTimeOffset at
    ) =>
        new()
        {
            ["id"] = id,
            ["parentCommentId"] = parent,
            ["author"] = FakeAzureDevOps.Ref(author),
            ["content"] = content,
            ["commentType"] = "text",
            ["publishedDate"] = at.ToString("O"),
            ["lastUpdatedDate"] = at.ToString("O"),
        };

    private static JsonObject Status(
        int iteration,
        string state,
        string genre,
        string name,
        DateTimeOffset at
    ) =>
        new()
        {
            ["id"] = iteration,
            ["state"] = state,
            ["iterationId"] = iteration,
            ["context"] = new JsonObject { ["genre"] = genre, ["name"] = name },
            ["creationDate"] = at.ToString("O"),
            ["updatedDate"] = at.ToString("O"),
        };

    private static async Task<JsonObject> LeaseAsync(
        HttpClient worker,
        string kind,
        Func<JsonObject, bool> mine
    )
    {
        for (var i = 0; i < 50; i++)
        {
            var lease = await worker.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "ado-test",
                    version = "test",
                    kinds = new[] { kind },
                },
                Ct
            );
            Assert.Equal(HttpStatusCode.OK, lease.StatusCode);
            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            if (mine(job))
                return job;
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/complete",
                    new
                    {
                        workerId = "ado-test",
                        result = new
                        {
                            rewrites = Array.Empty<object>(),
                            reviewChanges = Array.Empty<object>(),
                        },
                    },
                    Ct
                )
            ).EnsureSuccessStatusCode();
        }

        throw new InvalidOperationException($"No {kind} job for this test.");
    }

    private static async Task RefreshAsync(HttpClient admin, string repo) =>
        (
            await admin.PostAsJsonAsync("/api/v1/steering/refresh", new { repo }, Ct)
        ).EnsureSuccessStatusCode();

    private static async Task SetPromptModeAsync(HttpClient admin)
    {
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>(
            "/api/v1/org",
            Json.Options,
            Ct
        );
        if (org!.Settings.PromptMode is null)
            (
                await admin.PutAsJsonAsync(
                    "/api/v1/org/settings",
                    org.Settings with
                    {
                        PromptMode = PromptMode.Redacted,
                    },
                    Json.Options,
                    Ct
                )
            ).EnsureSuccessStatusCode();
    }

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
