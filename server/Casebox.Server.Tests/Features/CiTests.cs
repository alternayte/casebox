using System.Net;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Ci;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// Step 11 on the server (docs/specs/harness-ci.md): the nightly baseline scores what is missing,
// a pull request runs only its candidate against that cached score, a newer push cancels the older
// one, and the verdict reaches the pull request as one comment however often QueueBox delivers it.
[Collection(GitHubPolling.Name)]
public sealed class CiTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private const string Repo = "github.com/acme/harness-ci";
    internal const string Hash = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh";

    internal static readonly object Spec = new
    {
        agent = "claude-code",
        agentVersion = "2.1.0",
        model = "claude-sonnet-5-20260801",
        harness = "HEAD",
    };

    internal static readonly object Prices = new Dictionary<string, object>
    {
        ["claude-sonnet-5-20260801"] = new
        {
            input = 3m,
            output = 15m,
            cacheRead = 0.3m,
        },
    };

    [Fact]
    public async Task A_pull_request_runs_its_candidate_against_the_nightly_baseline_and_comments_once()
    {
        var workspace = await EvaluationTests.ApprovedCasesAsync(stack, 10, Repo);
        var caseIndex = Enumerable
            .Range(0, 10)
            .ToDictionary(i => Case.IdFor(StackFixture.OrgA, $"test:{workspace}:{i}"), i => i);
        var admin = await stack.ServerA.AdminAsync();
        (
            await admin.PutAsJsonAsync(
                "/api/v1/integrations/github",
                new { mode = "token", token = FakeServices.GitHubToken },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var ci = await stack.ServerA.TokenClientAsync(TokenKind.Ci);

        // A ci token reads nothing else of the API, and the refusal says so with its code.
        var forbidden = await ci.GetAsync("/api/v1/evaluations/", Ct);
        Assert.Equal(HttpStatusCode.Forbidden, forbidden.StatusCode);
        Assert.Equal(
            "CBX011",
            (await forbidden.Content.ReadFromJsonAsync<JsonElement>(Ct))
                .GetProperty("code")
                .GetString()
        );

        // The nightly baseline: every case is missing, so all ten are scored, 2 repeats each. The
        // baseline passes every case but the last.
        var baseline = await StartBaselineAsync(ci, workspace);
        Assert.Equal(
            HttpStatusCode.Forbidden,
            (await Lease(ci, null, [CiEndpoints.ResolveJob])).StatusCode
        );
        var scored = await PlayAsync(
            ci,
            baseline,
            (_, caseId) => caseIndex[caseId] != 9,
            run => run.GetProperty("status").GetString() == "done"
        );
        var score = scored.GetProperty("scored");
        Assert.Equal(
            (10, 20, 0.9),
            (
                score.GetProperty("cases").GetInt32(),
                score.GetProperty("runs").GetInt32(),
                score.GetProperty("passRate").GetDouble()
            )
        );

        // The next night nothing changed: the score is fresh and nothing runs.
        var again = await StartBaselineAsync(ci, workspace);
        var skipped = await PlayAsync(
            ci,
            again,
            (_, _) => true,
            run => run.GetProperty("status").GetString() == "skipped"
        );
        Assert.Contains("fresh", skipped.GetProperty("message").GetString());

        // A pull request's first push, then a second push: the first is cancelled.
        var first = await StartPullRequestAsync(ci, workspace, "1111111111111111");
        var second = await StartPullRequestAsync(ci, workspace, "2222222222222222");
        var firstRun = await ci.GetFromJsonAsync<JsonElement>($"/api/v1/ci/runs/{first}", Ct);
        Assert.Equal("cancelled", firstRun.GetProperty("status").GetString());

        // Only the candidate runs, with the pull request's harness. It fails case 0, which the
        // baseline passed in every run: a regression.
        var harnesses = new HashSet<string>();
        var done = await PlayAsync(
            ci,
            second,
            (payload, caseId) =>
            {
                if (payload["spec"] is JsonObject spec)
                    harnesses.Add(spec["harness"]!.GetValue<string>());
                return caseIndex[caseId] != 0;
            },
            run => run.GetProperty("status").GetString() == "done"
        );
        Assert.Equal([$"{Repo}@2222222222222222"], harnesses);
        Assert.Equal("harness_ci", done.GetProperty("purpose").GetString());
        var cases = done.GetProperty("cases").EnumerateArray().ToList();
        Assert.Equal(10, cases.Count);
        Assert.All(cases, c => Assert.Equal(1, c.GetProperty("candidateRuns").GetInt32()));
        var regression = Assert.Single(cases, c => c.GetProperty("regression").GetBoolean());
        Assert.Equal(0, caseIndex[regression.GetProperty("caseId").GetString()!]);
        Assert.Equal(
            (2, 2),
            (
                regression.GetProperty("baselinePassed").GetInt32(),
                regression.GetProperty("baselineRuns").GetInt32()
            )
        );
        var verdict = done.GetProperty("verdict");
        Assert.NotEqual("better", verdict.GetProperty("verdict").GetString());
        Assert.NotEqual("equivalent", verdict.GetProperty("verdict").GetString());

        // The verdict left through the outbox: one comment on the pull request.
        var comments = stack.Fakes.Repo("acme/harness-ci").IssueComments;
        await Eventually(() =>
        {
            lock (comments)
                return comments.Count == 1;
        });
        JsonObject comment;
        lock (comments)
            comment = comments.Single();
        var body = comment["body"]!.GetValue<string>();
        Assert.Contains(CiCommentEffect.Marker(workspace), body);
        Assert.Contains("**1 regression.**", body);
        Assert.Contains("casebox compare --candidate harness=2222222222222222", body);

        // QueueBox delivers the same message again: GitHub still holds one comment, updated.
        var evaluationId = done.GetProperty("evaluationId").GetString()!;
        using var management = stack.ServerA.Management();
        management.DefaultRequestHeaders.Add(
            EffectEndpoints.TokenHeader,
            StackFixture.EffectsToken
        );
        using var redelivery = new HttpRequestMessage(
            HttpMethod.Post,
            "/internal/effects/ci-comment"
        );
        redelivery.Content = JsonContent.Create(new { evaluationId });
        redelivery.Headers.Add("x-deedbox-tenant-id", StackFixture.OrgA);
        (await management.SendAsync(redelivery, Ct)).EnsureSuccessStatusCode();
        lock (comments)
        {
            Assert.Single(comments);
            Assert.True(comments[0]["edits"]?.GetValue<int>() >= 1);
        }

        // The outbox row was written once, with the evaluation's verdict.
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        Assert.Equal(
            1,
            await db.ExecuteScalarAsync<int>(
                "SELECT count(*) FROM outbox WHERE topic = @Topic AND payload->>'evaluationId' = @Id",
                new { Topic = CiCommentEffect.Topic, Id = evaluationId }
            )
        );
    }

    [Fact]
    public async Task A_pull_request_without_a_baseline_score_is_skipped_with_what_to_do()
    {
        var workspace = await EvaluationTests.ApprovedCasesAsync(stack, 2, Repo);
        var ci = await stack.ServerA.TokenClientAsync(TokenKind.Ci);
        var started = await (
            await ci.PostAsJsonAsync("/api/v1/ci/pull-requests", PullRequest(workspace, "3333"), Ct)
        ).Content.ReadFromJsonAsync<JsonElement>(Ct);
        var run = Assert.Single(started.GetProperty("runs").EnumerateArray());
        Assert.Equal("skipped", run.GetProperty("status").GetString());
        Assert.Contains("casebox ci --baseline", run.GetProperty("message").GetString());

        // A shared harness spec with an agent that has no user-level configuration is refused.
        var cursor = PullRequest(workspace, "4444");
        cursor["spec"] = new JsonObject
        {
            ["agent"] = "cursor-cli",
            ["agentVersion"] = "2026.06.19",
            ["model"] = "claude-sonnet-5-20260801",
            ["harness"] = "HEAD",
            ["shared"] = new JsonObject { ["repo"] = "github.com/acme/shared", ["ref"] = "HEAD" },
        };
        Assert.Equal(
            HttpStatusCode.UnprocessableEntity,
            (await ci.PostAsJsonAsync("/api/v1/ci/pull-requests", cursor, Ct)).StatusCode
        );
    }

    private static JsonObject PullRequest(string workspace, string head) =>
        new()
        {
            ["workspace"] = workspace,
            ["repo"] = Repo,
            ["number"] = 12,
            ["headSha"] = head,
            ["baseSha"] = "0000",
            ["spec"] = JsonSerializer.SerializeToNode(Spec),
            ["size"] = 10,
            ["repeats"] = 1,
            ["prices"] = JsonSerializer.SerializeToNode(Prices),
            ["serverUrl"] = "https://casebox.example.com",
            ["globs"] = new JsonArray("AGENTS.md", ".claude/skills/**"),
        };

    internal static async Task<string> StartBaselineAsync(HttpClient ci, string workspace)
    {
        var response = await ci.PostAsJsonAsync(
            "/api/v1/ci/baselines",
            new
            {
                workspace,
                spec = Spec,
                repeats = 2,
                prices = Prices,
                globs = new[] { "AGENTS.md" },
            },
            Ct
        );
        Assert.Equal(HttpStatusCode.Accepted, response.StatusCode);
        var started = await response.Content.ReadFromJsonAsync<JsonElement>(Ct);
        return Assert
            .Single(started.GetProperty("runs").EnumerateArray())
            .GetProperty("id")
            .GetString()!;
    }

    private static async Task<string> StartPullRequestAsync(
        HttpClient ci,
        string workspace,
        string head
    )
    {
        var response = await ci.PostAsJsonAsync(
            "/api/v1/ci/pull-requests",
            PullRequest(workspace, head),
            Ct
        );
        response.EnsureSuccessStatusCode();
        var run = Assert.Single(
            (await response.Content.ReadFromJsonAsync<JsonElement>(Ct))
                .GetProperty("runs")
                .EnumerateArray()
        );
        Assert.Equal("started", run.GetProperty("status").GetString());
        return run.GetProperty("id").GetString()!;
    }

    private static Task<HttpResponseMessage> Lease(HttpClient ci, string? scope, string[] kinds) =>
        ci.PostAsJsonAsync(
            "/worker/v1/jobs/lease",
            new
            {
                workerId = "ci-test",
                version = "t",
                kinds,
                scope,
            },
            Ct
        );

    // Plays the inline worker of one CI run until the run reaches the state: it answers the
    // resolve job with one harness hash for every case, each run, and each verify by passes.
    internal static async Task<JsonElement> PlayAsync(
        HttpClient ci,
        string ciRun,
        Func<JsonObject, string, bool> passes,
        Func<JsonElement, bool> finished
    )
    {
        var specs = new Dictionary<string, JsonObject>();
        var deadline = DateTime.UtcNow.AddSeconds(120);
        while (true)
        {
            var answer = await ci.GetAsync($"/api/v1/ci/runs/{ciRun}", Ct);
            var text = await answer.Content.ReadAsStringAsync(Ct);
            Assert.True(answer.IsSuccessStatusCode, $"GET the CI run: {answer.StatusCode} {text}");
            var run = JsonDocument.Parse(text).RootElement;
            if (finished(run))
                return run;
            Assert.True(DateTime.UtcNow < deadline, "The CI run did not finish: " + run);

            var lease = await Lease(ci, ciRun, [CiEndpoints.ResolveJob, "run", "verify"]);
            if (lease.StatusCode == HttpStatusCode.NoContent)
            {
                await Task.Delay(200, Ct);
                continue;
            }
            lease.EnsureSuccessStatusCode();
            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            var payload = (JsonObject)job["payload"]!;
            Assert.Equal(ciRun, payload["ciRun"]!.GetValue<string>());
            object result = job["kind"]!.GetValue<string>() switch
            {
                CiEndpoints.ResolveJob => new
                {
                    hashes = payload["cases"]!
                        .AsArray()
                        .ToDictionary(c => c!["caseId"]!.GetValue<string>(), _ => Hash),
                },
                "run" => RunAnswer(payload, specs),
                _ => VerifyAnswer(payload, specs, passes),
            };
            (
                await ci.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/complete",
                    new { workerId = "ci-test", result },
                    Ct
                )
            ).EnsureSuccessStatusCode();
        }
    }

    private static object RunAnswer(JsonObject payload, Dictionary<string, JsonObject> specs)
    {
        var runId = payload["runId"]!.GetValue<string>();
        specs[runId] = payload;
        return new
        {
            runId,
            diff = new string('d', 64),
            usage = new { inputTokens = 1000, outputTokens = 200 },
            costUsd = 0.05m,
            seconds = 60.0,
            turns = 3,
            toolCalls = 5,
            model = "claude-sonnet-5-20260801",
            harnessHash = Hash,
        };
    }

    private static object VerifyAnswer(
        JsonObject payload,
        Dictionary<string, JsonObject> specs,
        Func<JsonObject, string, bool> passes
    )
    {
        var runId = payload["runId"]!.GetValue<string>();
        var passed = passes(specs[runId], payload["caseId"]!.GetValue<string>());
        return new
        {
            runId,
            applied = true,
            tests = new
            {
                failToPass = new { passed = passed ? 1 : 0, total = 1 },
                passToPass = new { passed = 2, total = 2 },
            },
            passed,
        };
    }

    private static async Task Eventually(Func<bool> condition)
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (!condition())
        {
            Assert.True(
                DateTime.UtcNow < deadline,
                "The condition did not hold within 60 seconds."
            );
            await Task.Delay(300, Ct);
        }
    }
}
