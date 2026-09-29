using System.Net;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Deedbox;
using Microsoft.Extensions.Caching.Memory;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// Step 7 end to end: sessions of a team become interventions, a worker classifies them through
// the job queue, and the report shows only what k people stand behind. Pull request signals come
// from the GitHub fake and a worker's steering.pr answer.
// The GitHub poller polls every repository of organisation A, and its cursors are shared, so
// the test classes that poll run one after the other.
[Collection(GitHubPolling.Name)]
public sealed partial class SteeringTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [GeneratedRegex("person:[a-z2-7]{26}")]
    private static partial Regex Token();

    private const string Mock =
        "no, don't mock the database in the integration test; use the real one";
    private const string WrongFile = "stop, you are editing the wrong file";

    [Fact]
    public async Task A_team_history_becomes_a_report_that_shows_only_what_k_people_stand_behind()
    {
        var admin = await stack.ServerA.AdminAsync();
        await SetPromptModeAsync(admin);
        stack
            .ServerA.Services.GetRequiredService<IMemoryCache>()
            .Remove($"roster:{StackFixture.OrgA}");
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        var tag = Guid.NewGuid().ToString("N")[..8];
        var repo = $"github.com/acme/steer-{tag}";
        var small = $"github.com/acme/steer-small-{tag}";
        var now = DateTimeOffset.UtcNow;

        var sessions = new Dictionary<string, string>();
        foreach (var person in new[] { "grace", "linus", "margaret" })
            sessions[person] = await IngestAsync(
                ingest,
                repo,
                person,
                "claude-code",
                now.AddHours(-5),
                now.AddHours(-3)
            );
        // Grace gives up on Claude Code and starts again with Codex half an hour later.
        var restart = await IngestAsync(
            ingest,
            repo,
            "grace",
            "codex",
            now.AddHours(-2.5),
            null,
            tasks: ["try again with a smaller change"]
        );
        foreach (var person in new[] { "grace", "linus" })
            await IngestAsync(
                ingest,
                small,
                person,
                "claude-code",
                now.AddHours(-5),
                now.AddHours(-3)
            );

        var status = await RefreshAsync(admin, repo);
        // Per session: a follow-up and an interruption with text, a denial without, and the
        // abandonment; plus Grace's restart.
        Assert.Equal(
            (13, 7),
            (
                status.GetProperty("interventions").GetInt32(),
                status.GetProperty("pending").GetInt32()
            )
        );

        // A waiting task-type job for a session does not hold back its interventions' job.
        await using (var db = new NpgsqlConnection(stack.ConnectionString))
            Assert.Equal(
                3,
                await db.QuerySingleAsync<int>(
                    """
                    SELECT count(DISTINCT payload->>'stream') FROM casebox.jobs
                    WHERE org_id = @Org AND kind = 'steering.classify' AND jsonb_array_length(payload->'interventionIds') > 0 AND payload->>'stream' = ANY(@Streams)
                    """,
                    new
                    {
                        Org = StackFixture.OrgA,
                        Streams = sessions.Values.Select(id => $"steering:session:{id}").ToArray(),
                    }
                )
            );

        await ClassifyAllAsync(
            admin,
            worker,
            [repo, small],
            window =>
            {
                var human = window["human"]?.GetValue<string>() ?? "";
                Assert.DoesNotMatch(Token(), window.ToJsonString());
                if (human.Contains("don't mock", StringComparison.Ordinal))
                {
                    Assert.Contains("[person]", human, StringComparison.Ordinal);
                    Assert.Equal(
                        "I added retries to the client.",
                        window["before"]!["text"]!.GetValue<string>()
                    );
                    Assert.Equal(
                        "Understood, switching to the real database.",
                        window["after"]!["text"]!.GetValue<string>()
                    );
                    return Label("correction", "broke_convention", "instruction", 0.92);
                }

                if (human.Contains("wrong file", StringComparison.Ordinal))
                    return Label("correction", "wrong_area", "verification", 0.4);
                if (window["signal"]!.GetValue<string>() == "restarted")
                    return Label("direction", "wrong_approach", "stronger_model", 0.8);
                return Label("direction", null, null, 0.9);
            }
        );

        var report = await ReportAsync(admin, repo);
        var theme = report
            .GetProperty("themes")
            .EnumerateArray()
            .Single(t => t.GetProperty("wentWrong").GetString() == "broke_convention");
        Assert.Equal(
            (3, 3, 3),
            (
                theme.GetProperty("corrections").GetInt32(),
                theme.GetProperty("people").GetInt32(),
                theme.GetProperty("quotes").GetArrayLength()
            )
        );
        Assert.True(theme.GetProperty("harnessFixable").GetBoolean());
        Assert.All(
            theme.GetProperty("quotes").EnumerateArray(),
            q => Assert.Matches(@"^\d{4}-\d{2}-\d{2}$", q.GetProperty("day").GetString()!)
        );
        Assert.DoesNotContain(
            "wrong_area",
            report
                .GetProperty("themes")
                .EnumerateArray()
                .Select(t => t.GetProperty("wentWrong").GetString())
        );
        // Three denials without text and three interruptions below the confidence threshold.
        Assert.Equal(6, report.GetProperty("coverage").GetProperty("unclassified").GetInt32());
        var abandonment = report.GetProperty("headline").GetProperty("abandonmentRate");
        Assert.Equal(
            (1.0, 3),
            (abandonment.GetProperty("value").GetDouble(), abandonment.GetProperty("n").GetInt32())
        );
        Assert.DoesNotMatch(Token(), report.GetRawText());

        // The restart is a correction by rule, whatever the model said about its intent.
        await using (var db = new NpgsqlConnection(stack.ConnectionString))
        {
            var restarted = await db.QuerySingleAsync<(
                string Intent,
                string LabelSource,
                string WentWrong
            )>(
                "SELECT intent, label_source, went_wrong FROM casebox.steering_facts WHERE org_id = @Org AND session_id = @Id AND signal = 'restarted'",
                new { Org = StackFixture.OrgA, Id = sessions["grace"] }
            );
            Assert.Equal(("correction", "rule", "wrong_approach"), restarted);
            Assert.Equal(
                "feature",
                await db.QuerySingleAsync<string>(
                    "SELECT task_type FROM casebox.sessions WHERE org_id = @Org AND id = @Id",
                    new { Org = StackFixture.OrgA, Id = restart }
                )
            );
        }

        // Two people are fewer than k: the smaller repository shows no theme and no interventions.
        var smallReport = await ReportAsync(admin, small);
        Assert.Empty(smallReport.GetProperty("themes").EnumerateArray());
        Assert.True(smallReport.GetProperty("hidden").GetProperty("themes").GetInt32() >= 1);
        Assert.Equal(
            JsonValueKind.Null,
            smallReport.GetProperty("headline").GetProperty("abandonmentRate").ValueKind
        );
        Assert.Equal(
            HttpStatusCode.Forbidden,
            (
                await admin.GetAsync(
                    $"/api/v1/steering/interventions?wentWrong=broke_convention&repo={small}",
                    Ct
                )
            ).StatusCode
        );

        // A relabel from the theme's list wins, and becomes the classifier's example.
        var listed = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/steering/interventions?wentWrong=broke_convention&repo={repo}",
            Ct
        );
        Assert.Equal(3, listed.GetProperty("total").GetInt32());
        var first = listed.GetProperty("interventions")[0];
        Assert.Equal("model", first.GetProperty("labelSource").GetString());
        (
            await admin.PostAsJsonAsync(
                "/api/v1/steering/relabel",
                new
                {
                    @ref = first.GetProperty("ref").GetString(),
                    intent = "correction",
                    wentWrong = "broke_convention",
                    prevention = "verification",
                },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var examples = await worker.GetFromJsonAsync<JsonElement>(
            "/worker/v1/steering/examples",
            Ct
        );
        Assert.Contains(
            examples.GetProperty("examples").EnumerateArray(),
            e =>
                e.GetProperty("prevention").GetString() == "verification"
                && e.GetProperty("window")
                    .GetProperty("human")
                    .GetString()!
                    .Contains("don't mock", StringComparison.Ordinal)
        );

        // Rebuilding the projection gives the same report once the replay reaches the head.
        var period =
            $"&from={now.AddDays(-2):yyyy-MM-ddTHH:mm:ssZ}&to={now.AddHours(1):yyyy-MM-ddTHH:mm:ssZ}";
        var before = (await ReportAsync(admin, repo + period)).GetRawText();
        await RebuildAsync();
        var after = "";
        for (
            var deadline = DateTime.UtcNow.AddSeconds(60);
            DateTime.UtcNow < deadline && after != before;
            await Task.Delay(300, Ct)
        )
            after = (await ReportAsync(admin, repo + period)).GetRawText();
        Assert.Equal(before, after);

        // Erasing one person removes their interventions: the theme falls below k.
        (
            await admin.PostAsJsonAsync(
                "/api/v1/privacy/erasures",
                new { identity = $"email:{TestRoster.TeamEmail("margaret")}" },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var erased = await ReportAsync(admin, repo);
        Assert.DoesNotContain(
            "broke_convention",
            erased
                .GetProperty("themes")
                .EnumerateArray()
                .Select(t => t.GetProperty("wentWrong").GetString())
        );
        await using (var db = new NpgsqlConnection(stack.ConnectionString))
            Assert.Equal(
                0,
                await db.QuerySingleAsync<int>(
                    "SELECT count(*) FROM casebox.steering_facts WHERE org_id = @Org AND session_id = @Id",
                    new { Org = StackFixture.OrgA, Id = sessions["margaret"] }
                )
            );
    }

    [Fact]
    public async Task Pull_request_signals_come_from_checks_reverts_and_the_workers_rewrite_answer()
    {
        var fakes = stack.Fakes;
        var tag = Guid.NewGuid().ToString("N")[..8];
        var name = $"acme/steer-pr-{tag}";
        var repo = $"github.com/{name}";
        var now = DateTimeOffset.UtcNow;
        var fake = fakes.Repo(name);

        var agentSha = FakePull.Sha();
        var fixSha = FakePull.Sha();
        var pr = new FakePull(21)
        {
            Title = "Add export",
            HeadRef = "export",
            HeadSha = fixSha,
            AuthorLogin = "grace-team",
            CreatedAt = now.AddDays(-6),
            UpdatedAt = now.AddDays(-5),
            MergedAt = now.AddDays(-5),
        };
        pr.Commits.Add(
            Commit(
                agentSha,
                "add export\n\nCo-authored-by: Claude <noreply@anthropic.com>",
                "grace-team",
                now.AddDays(-6)
            )
        );
        pr.Commits.Add(Commit(fixSha, "fix the flaky assertion", "linus-team", now.AddDays(-5.8)));
        pr.Checks.Add(
            new JsonObject
            {
                ["name"] = "test",
                ["conclusion"] = "failure",
                ["head_sha"] = agentSha,
                ["completed_at"] = now.AddDays(-5.9).ToString("O"),
            }
        );
        pr.Checks.Add(
            new JsonObject
            {
                ["name"] = "test",
                ["conclusion"] = "success",
                ["head_sha"] = fixSha,
                ["completed_at"] = now.AddDays(-5.7).ToString("O"),
            }
        );
        pr.Comments.Add(
            new JsonObject
            {
                ["id"] = 501,
                ["path"] = "src/export.go",
                ["line"] = null,
                ["original_line"] = 10,
                ["commit_id"] = fixSha,
                ["original_commit_id"] = agentSha,
                ["user"] = User("margaret-team"),
                ["created_at"] = now.AddDays(-5.85).ToString("O"),
                ["body"] = "please don't swallow the error here",
            }
        );
        fake.Pulls[21] = pr;
        fake.Pulls[22] = new FakePull(22)
        {
            Title = "Revert \"Add export\"",
            Body = $"Reverts {name}#21",
            HeadRef = "revert-21",
            AuthorLogin = "edsger-team",
            CreatedAt = now.AddDays(-4.5),
            UpdatedAt = now.AddDays(-4),
            MergedAt = now.AddDays(-4),
        };

        var admin = await stack.ServerA.AdminAsync();
        await SetPromptModeAsync(admin);
        var workspace = $"steer-{tag}";
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync($"/api/v1/workspaces/{workspace}/repos", new { repo }, Ct)
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

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await Eventually(async () =>
            await db.QuerySingleAsync<int>(
                "SELECT count(*) FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo",
                new { Org = StackFixture.OrgA, Repo = repo }
            ) == 2
        );
        await RefreshAsync(admin, repo);

        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
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
        var comment = Assert.Single(payload["comments"]!.AsArray());
        Assert.Equal(
            (501L, 10, agentSha),
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
                    workerId = "steer-test",
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
                        reviewChanges = new[] { new { commentId = 501, sha = fixSha } },
                    },
                },
                Ct
            )
        ).EnsureSuccessStatusCode();

        var facts = (
            await db.QueryAsync<(
                string Id,
                string Signal,
                string Phase,
                string? Intent,
                string? Text
            )>(
                "SELECT intervention_id, signal, phase, intent, text FROM casebox.steering_facts WHERE org_id = @Org AND repo = @Repo ORDER BY intervention_id",
                new { Org = StackFixture.OrgA, Repo = repo }
            )
        ).ToList();
        Assert.Equal(
            [
                ("ci_fix", "before_merge", "correction"),
                ("human_rewrite", "before_merge", "correction"),
                ("revert", "after_merge", "correction"),
                ("review_change", "before_merge", null),
            ],
            facts
                .OrderBy(f => f.Signal, StringComparer.Ordinal)
                .Select(f => (f.Signal, f.Phase, f.Intent))
                .ToList()
        );
        Assert.Contains(
            "swallow the error",
            facts.Single(f => f.Signal == "review_change").Text,
            StringComparison.Ordinal
        );

        var found = await PrivacyScan.FindAsync(
            stack,
            ["grace-team", "linus-team", "margaret-team", "edsger-team"]
        );
        Assert.True(found.Count == 0, "Identities found in: " + string.Join(", ", found));
    }

    private static JsonObject Label(
        string intent,
        string? wentWrong,
        string? prevention,
        double confidence
    ) =>
        new()
        {
            ["intent"] = intent,
            ["wentWrong"] = wentWrong,
            ["prevention"] = prevention,
            ["confidence"] = confidence,
        };

    private static async Task<string> IngestAsync(
        HttpClient ingest,
        string repo,
        string person,
        string agent,
        DateTimeOffset start,
        DateTimeOffset? end,
        string[]? tasks = null
    )
    {
        var id = $"{agent}:{Guid.NewGuid()}";
        var t = start;
        CapturedEvent E(
            long seq,
            string kind,
            string? text = null,
            CapturedTool? tool = null,
            Dictionary<string, string>? attrs = null
        ) => new(seq, t = t.AddMinutes(1), kind, text, tool, null, attrs);
        List<CapturedEvent> events = tasks is not null
            ? [E(0, "prompt", tasks[0]), E(1, "response", "Done.")]
            :
            [
                E(0, "prompt", "add retries to the payment client"),
                E(1, "response", "I added retries to the client."),
                E(2, "tool_call", tool: new CapturedTool("Edit", "ok", ["client.go"])),
                E(3, "prompt", $"{Mock}, ask ⟦cbx:email:{TestRoster.TeamEmail("edsger")}⟧"),
                E(4, "response", "Understood, switching to the real database."),
                E(5, "interruption"),
                E(6, "prompt", WrongFile),
                E(7, "tool_call", tool: new CapturedTool("Edit", "ok", ["db_test.go"])),
                E(
                    8,
                    "denial",
                    tool: new CapturedTool("Bash", "denied", null),
                    attrs: new() { ["denial"] = "user-rejected" }
                ),
                E(9, "tool_call", tool: new CapturedTool("Read", "ok", ["db_test.go"])),
            ];
        var batch = new CaptureBatch(
            new CapturedSession(
                id,
                agent,
                "1.0.0",
                agent == "codex" ? "gpt-5.5" : "claude-sonnet-5",
                repo,
                "feature-retries",
                null,
                null,
                start,
                end,
                "import",
                null,
                $"⟦cbx:email:{TestRoster.TeamEmail(person)}⟧",
                new CapturedHarness(new string('a', 64), ["AGENTS.md"]),
                end is null ? null : 0
            ),
            events
        );
        (
            await ingest.PostAsJsonAsync("/ingest/v1/sessions", batch, Json.Options, Ct)
        ).EnsureSuccessStatusCode();
        return id;
    }

    // Plays the worker: leases every classification job, reads its windows, and answers.
    private static async Task ClassifyAllAsync(
        HttpClient admin,
        HttpClient worker,
        string[] repos,
        Func<JsonObject, JsonObject> label
    )
    {
        for (var idle = 0; idle < 3; )
        {
            var lease = await worker.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "steer-test",
                    version = "test",
                    kinds = new[] { "steering.classify" },
                },
                Ct
            );
            if (lease.StatusCode == HttpStatusCode.NoContent)
            {
                var pending = 0;
                foreach (var repo in repos)
                    pending += (await RefreshAsync(admin, repo)).GetProperty("pending").GetInt32();
                if (pending == 0)
                    return;
                idle++;
                continue;
            }

            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            var windows = (
                await (
                    await worker.PostAsJsonAsync("/worker/v1/steering/windows", job["payload"], Ct)
                ).Content.ReadFromJsonAsync<JsonObject>(Ct)
            )!;
            var results = windows["windows"]!
                .AsArray()
                .Select(w =>
                {
                    var answer = label(w!.AsObject());
                    answer["interventionId"] = w["interventionId"]!.GetValue<string>();
                    return answer;
                })
                .ToArray();
            var taskType = windows["task"] is JsonObject ? "feature" : null;
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/complete",
                    new
                    {
                        workerId = "steer-test",
                        result = new
                        {
                            model = "test-model",
                            promptVersion = "steering-v1",
                            results,
                            taskType,
                        },
                    },
                    Ct
                )
            ).EnsureSuccessStatusCode();
        }

        Assert.Fail("Interventions stayed pending with no job to lease.");
    }

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
                    workerId = "steer-test",
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
                        workerId = "steer-test",
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

    private static async Task<JsonElement> RefreshAsync(HttpClient admin, string repo)
    {
        var response = await admin.PostAsJsonAsync("/api/v1/steering/refresh", new { repo }, Ct);
        response.EnsureSuccessStatusCode();
        return await response.Content.ReadFromJsonAsync<JsonElement>(Ct);
    }

    private static Task<JsonElement> ReportAsync(HttpClient admin, string repo) =>
        admin.GetFromJsonAsync<JsonElement>($"/api/v1/steering/report?repo={repo}", Ct);

    private async Task RebuildAsync()
    {
        using var scope = stack.ServerA.Services.CreateScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
        var admin = scope.ServiceProvider.GetRequiredService<IEventStoreAdmin>();
        var job = await admin.RebuildAsync("steering_facts", Ct);
        await Eventually(async () =>
            (await admin.GetJobAsync(job, Ct)) is { FinishedAt: not null } info
            && info.Status == "done"
        );
    }

    private static JsonObject Commit(string sha, string message, string login, DateTimeOffset at) =>
        new()
        {
            ["sha"] = sha,
            ["author"] = User(login),
            ["commit"] = new JsonObject
            {
                ["message"] = message,
                ["author"] = new JsonObject
                {
                    ["name"] = login,
                    ["email"] = $"{login}@users.noreply.example.com",
                    ["date"] = at.ToString("O"),
                },
                ["committer"] = new JsonObject
                {
                    ["name"] = login,
                    ["email"] = $"{login}@users.noreply.example.com",
                    ["date"] = at.ToString("O"),
                },
            },
        };

    private static JsonObject User(string login) => new() { ["login"] = login, ["type"] = "User" };

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
