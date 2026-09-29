using System.Net;
using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Deedbox;
using Deedbox.Testing;
using Microsoft.Extensions.DependencyInjection;

namespace Casebox.Server.Tests.Features;

public sealed class EvaluationDeciderTests
{
    private static readonly HarnessSpec Baseline = new(
        "claude-code",
        "2.1.0",
        "claude-sonnet-5-20260801",
        null,
        "main",
        new AgentSettings(200, 30, 2_000_000),
        null
    );

    private static readonly EvaluationEvents.Requested Requested = new(
        "payments",
        "dev",
        [new EvaluationCase("c1", 1)],
        Baseline,
        Baseline with
        {
            Harness = "none",
        },
        "harness",
        3,
        0.05,
        10m,
        new Estimate(1, 6, 1, 1, 1m, 1m, 2m, 60, 20, 60, 0.67m, null),
        Purpose.HarnessVsNone,
        false,
        false,
        new Dictionary<string, Price>()
    );

    private static EvaluationEvents.RunCompleted Completed(string id, decimal cost) =>
        new(id, "c1", Side.Baseline, 1, true, cost, 60, 1000, "strong");

    [Fact]
    public void Two_sides_that_differ_in_two_things_are_not_one_evaluation() =>
        Assert.Equal(
            ["model", "harness"],
            HarnessSpec.Changes(Baseline, Baseline with { Model = "other-2026", Harness = "none" })
        );

    [Fact]
    public void A_run_is_recorded_once() =>
        Decider
            .Given<Evaluation>(Requested, Completed("c1:b:1", 1))
            .When(e => EvaluationDecider.CompleteRun(e, Completed("c1:b:1", 1)))
            .ThenNothing();

    [Fact]
    public void A_run_that_takes_the_spend_past_the_cap_stops_the_evaluation() =>
        Decider
            .Given<Evaluation>(Requested, Completed("c1:b:1", 9))
            .When(e => EvaluationDecider.CompleteRun(e, Completed("c1:c:1", 2)))
            .Then(Completed("c1:c:1", 2), new EvaluationEvents.BudgetExhausted(11));

    [Fact]
    public void No_run_is_recorded_after_a_verdict() =>
        Decider
            .Given<Evaluation>(
                Requested,
                new EvaluationEvents.VerdictReached(
                    Statistics.Verdict.Inconclusive,
                    0,
                    -1,
                    1,
                    0.95,
                    1,
                    1,
                    null,
                    null,
                    null,
                    null,
                    null,
                    null,
                    false,
                    0,
                    0,
                    null
                )
            )
            .When(e => EvaluationDecider.CompleteRun(e, Completed("c1:c:1", 1)))
            .ThenThrows<ConflictException>();

    [Fact]
    public void No_verdict_is_given_below_ten_cases() =>
        Decider
            .Given<Evaluation>(Requested)
            .When(e =>
                EvaluationDecider.Conclude(
                    e,
                    new EvaluationEvents.VerdictReached(
                        Statistics.Verdict.Better,
                        0.5,
                        0.2,
                        0.8,
                        0.95,
                        9,
                        18,
                        null,
                        null,
                        null,
                        null,
                        null,
                        null,
                        false,
                        0.2,
                        0.7,
                        null
                    )
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void A_checkpoint_needs_its_round_finished() =>
        Decider
            .Given<Evaluation>(Requested, Completed("c1:b:1", 1))
            .When(e =>
                EvaluationDecider.Checkpoint(
                    e,
                    new EvaluationEvents.CheckpointEvaluated(
                        1,
                        0.99,
                        1,
                        0,
                        0,
                        0,
                        Statistics.Verdict.Inconclusive
                    )
                )
            )
            .ThenThrows<DomainException>();

    [Fact]
    public void Another_agent_with_its_own_version_is_one_change() =>
        Assert.Equal(
            ["agent"],
            HarnessSpec.Changes(
                Baseline,
                Baseline with
                {
                    Agent = "codex",
                    AgentVersion = "0.153.4",
                }
            )
        );

    [Fact]
    public void Aliases_and_latest_are_mutable_models() =>
        Assert.Equal(
            [true, true, false, false],
            new[]
            {
                "sonnet",
                "gpt-5-latest",
                "claude-sonnet-5-20260801",
                "deepseek/deepseek-v3.2",
            }.Select(HarnessSpec.MutableModel)
        );
}

// Step 10 on the server: an evaluation on ten approved cases runs round by round through the job
// queue, stops early once the verdict is clear, and never shows a held-out evaluation's cases.
public sealed class EvaluationTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private static readonly object Prices = new Dictionary<string, object>
    {
        ["claude-sonnet-5-20260801"] = new
        {
            input = 3m,
            output = 15m,
            cacheRead = 0.3m,
        },
    };

    [Fact]
    public async Task A_clear_improvement_stops_after_the_first_round_with_its_interval_and_cost()
    {
        var workspace = await ApprovedCasesAsync(10);
        var admin = await stack.ServerA.AdminAsync();
        var body = Request(workspace, repeats: 3);

        var estimate = await (
            await admin.PostAsJsonAsync("/api/v1/evaluations/estimate", body, Ct)
        ).Content.ReadFromJsonAsync<JsonElement>(Ct);
        Assert.Equal(
            (10, 60),
            (
                estimate.GetProperty("cases").GetInt32(),
                estimate.GetProperty("estimate").GetProperty("runs").GetInt32()
            )
        );
        Assert.Equal("harness", estimate.GetProperty("change").GetString());

        var created = await admin.PostAsJsonAsync("/api/v1/evaluations", body, Ct);
        Assert.Equal(HttpStatusCode.Created, created.StatusCode);
        var id = (await created.Content.ReadFromJsonAsync<JsonElement>(Ct))
            .GetProperty("id")
            .GetString()!;

        // The worker: the candidate (no harness here, the "change") passes every case, the baseline none.
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        await PlayAsync(worker, id, side => side == "candidate");

        var detail = await Eventually(
            admin,
            id,
            d => d.GetProperty("evaluation").GetProperty("status").GetString() == "done"
        );
        var verdict = detail.GetProperty("evaluation").GetProperty("verdict");
        Assert.Equal("better", verdict.GetProperty("verdict").GetString());
        Assert.Equal(
            (10, 20),
            (verdict.GetProperty("cases").GetInt32(), verdict.GetProperty("runs").GetInt32())
        );
        Assert.Equal(Statistics.Level(1, 3), verdict.GetProperty("level").GetDouble());
        Assert.Single(detail.GetProperty("checkpoints").EnumerateArray());
        Assert.Equal(20, detail.GetProperty("evaluation").GetProperty("runsCompleted").GetInt32());

        var cases = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/evaluations/{id}/cases",
            Ct
        );
        Assert.Equal(10, cases.GetArrayLength());
        Assert.All(
            cases.EnumerateArray(),
            c =>
                Assert.Equal(
                    (1, 0, 1, 1),
                    (
                        c.GetProperty("baselineRuns").GetInt32(),
                        c.GetProperty("baselinePassed").GetInt32(),
                        c.GetProperty("candidateRuns").GetInt32(),
                        c.GetProperty("candidatePassed").GetInt32()
                    )
                )
        );

        // Round 2 was never started.
        var lease = await worker.PostAsJsonAsync(
            "/worker/v1/jobs/lease",
            new
            {
                workerId = "eval-test",
                version = "t",
                kinds = new[] { "run" },
            },
            Ct
        );
        if (lease.StatusCode == HttpStatusCode.OK)
            Assert.NotEqual(
                id,
                (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!["payload"]![
                    "evaluationId"
                ]!.GetValue<string>()
            );
    }

    [Fact]
    public async Task An_estimate_over_the_threshold_waits_for_a_person_and_a_cancel_stops_the_rest()
    {
        var workspace = await ApprovedCasesAsync(10);
        var admin = await stack.ServerA.AdminAsync();
        var body = Request(
            workspace,
            repeats: 1,
            prices: new Dictionary<string, object>
            {
                ["claude-sonnet-5-20260801"] = new { input = 6m, output = 30m },
            }
        );
        var created = await (
            await admin.PostAsJsonAsync("/api/v1/evaluations", body, Ct)
        ).Content.ReadFromJsonAsync<JsonElement>(Ct);
        var id = created.GetProperty("id").GetString()!;
        Assert.True(created.GetProperty("estimate").GetProperty("needsConfirmation").GetBoolean());
        var waiting = await admin.GetFromJsonAsync<JsonElement>($"/api/v1/evaluations/{id}", Ct);
        Assert.Equal(
            "awaiting_confirmation",
            waiting.GetProperty("evaluation").GetProperty("status").GetString()
        );

        (
            await admin.PostAsync($"/api/v1/evaluations/{id}/confirmation", null, Ct)
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/evaluations/{id}/cancellation",
                new { reason = "changed my mind" },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var cancelled = await Eventually(
            admin,
            id,
            d => d.GetProperty("evaluation").GetProperty("status").GetString() == "cancelled"
        );
        Assert.Equal(
            "changed my mind",
            cancelled.GetProperty("evaluation").GetProperty("reason").GetString()
        );
    }

    [Fact]
    public async Task Two_changes_and_a_missing_price_are_refused()
    {
        var workspace = await ApprovedCasesAsync(1);
        var admin = await stack.ServerA.AdminAsync();
        var twoChanges = Request(workspace, repeats: 1);
        ((JsonObject)twoChanges["candidate"]!)["model"] = "claude-opus-5-20260801";
        Assert.Equal(
            HttpStatusCode.UnprocessableEntity,
            (await admin.PostAsJsonAsync("/api/v1/evaluations/estimate", twoChanges, Ct)).StatusCode
        );
        var noPrice = Request(workspace, repeats: 1, prices: new Dictionary<string, object>());
        Assert.Equal(
            HttpStatusCode.UnprocessableEntity,
            (await admin.PostAsJsonAsync("/api/v1/evaluations/estimate", noPrice, Ct)).StatusCode
        );
    }

    private static JsonObject Request(string workspace, int repeats, object? prices = null)
    {
        var baseline = new JsonObject
        {
            ["agent"] = "claude-code",
            ["agentVersion"] = "2.1.0",
            ["model"] = "claude-sonnet-5-20260801",
            ["harness"] = "main",
            ["settings"] = new JsonObject
            {
                ["maxTurns"] = 200,
                ["timeoutMinutes"] = 30,
                ["tokenCap"] = 2000000,
            },
        };
        var candidate = (JsonObject)baseline.DeepClone();
        candidate["harness"] = "none";
        return new JsonObject
        {
            ["workspace"] = workspace,
            ["baseline"] = baseline,
            ["candidate"] = candidate,
            ["repeats"] = repeats,
            ["purpose"] = "harness_vs_none",
            ["prices"] = JsonSerializer.SerializeToNode(prices ?? Prices),
        };
    }

    // Plays the worker for one evaluation: answers every run job, then every verify job.
    private static async Task PlayAsync(
        HttpClient worker,
        string evaluationId,
        Func<string, bool> passes
    )
    {
        var deadline = DateTime.UtcNow.AddSeconds(90);
        var verified = 0;
        while (verified < 20 && DateTime.UtcNow < deadline)
        {
            var lease = await worker.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "eval-test",
                    version = "t",
                    kinds = new[] { "run", "verify" },
                },
                Ct
            );
            if (lease.StatusCode == HttpStatusCode.NoContent)
            {
                await Task.Delay(200, Ct);
                continue;
            }

            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            var payload = job["payload"]!;
            if (payload["evaluationId"]!.GetValue<string>() != evaluationId)
            {
                (
                    await worker.PostAsJsonAsync(
                        $"/worker/v1/jobs/{job["id"]}/fail",
                        new
                        {
                            workerId = "eval-test",
                            error = "not this test's",
                            retryable = false,
                        },
                        Ct
                    )
                ).EnsureSuccessStatusCode();
                continue;
            }

            var runId = payload["runId"]!.GetValue<string>();
            object result =
                job["kind"]!.GetValue<string>() == "run"
                    ? new
                    {
                        runId,
                        diff = new string('d', 64),
                        usage = new
                        {
                            inputTokens = 1000,
                            outputTokens = 200,
                            cacheReadTokens = 5000,
                        },
                        costUsd = 0.05m,
                        seconds = 120.0,
                        turns = 4,
                        toolCalls = 9,
                        model = "claude-sonnet-5-20260801",
                        processChecks = new
                        {
                            ranTestsBeforeDone = true,
                            editedTestAfterFailure = false,
                        },
                        harnessHash = new string('h', 64),
                    }
                    : new
                    {
                        runId,
                        applied = true,
                        tests = new
                        {
                            failToPass = new
                            {
                                passed = passes(
                                    runId.Contains(":c:", StringComparison.Ordinal)
                                        ? "candidate"
                                        : "baseline"
                                )
                                    ? 1
                                    : 0,
                                total = 1,
                            },
                            passToPass = new { passed = 3, total = 3 },
                        },
                        passed = false,
                    };
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/complete",
                    new { workerId = "eval-test", result },
                    Ct
                )
            ).EnsureSuccessStatusCode();
            if (job["kind"]!.GetValue<string>() == "verify")
                verified++;
        }

        Assert.Equal(20, verified);
    }

    // Approved dev cases in a new workspace, written through the case stream as the review would.
    private async Task<string> ApprovedCasesAsync(int count)
    {
        var admin = await stack.ServerA.AdminAsync();
        var workspace = $"eval-{Guid.NewGuid():N}"[..20];
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/repos",
                new { repo = "github.com/acme/eval" },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var recipe = JsonDocument
            .Parse(
                """{"image":"golang:1.26","install":[],"lockfiles":[],"test":[{"command":"go test ./..."}],"services":{},"links":[]}"""
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

        using var scope = stack.ServerA.Services.CreateScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = StackFixture.OrgA;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        for (var i = 0; i < count; i++)
        {
            var id = Case.IdFor(StackFixture.OrgA, $"test:{workspace}:{i}");
            await store.Append(
                Case.StreamId(id),
                ExpectedVersion.Any,
                [
                    new CaseEvents.Mined(
                        CaseKind.Capability,
                        workspace,
                        $"test:{workspace}:{i}",
                        null,
                        CaseScope.Single,
                        [new CaseRepo("github.com/acme/eval", "base", "merged", RepoRole.Sealed)],
                        hash!,
                        new string('h', 64),
                        1
                    ),
                    new CaseEvents.Validated(new string('o', 64), 1, 3, 30, false, 1),
                    new CaseEvents.InstructionDrafted("Add retries.", "system", [], [], [], "m"),
                    new CaseEvents.Approved("acct"),
                    new CaseEvents.SplitAssigned(CaseSplit.Dev),
                ]
            );
        }

        return workspace;
    }

    private static async Task<JsonElement> Eventually(
        HttpClient admin,
        string id,
        Func<JsonElement, bool> done
    )
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (true)
        {
            var detail = await admin.GetFromJsonAsync<JsonElement>($"/api/v1/evaluations/{id}", Ct);
            if (done(detail))
                return detail;
            Assert.True(
                DateTime.UtcNow < deadline,
                "The evaluation did not reach the expected state within 60 seconds: " + detail
            );
            await Task.Delay(300, Ct);
        }
    }
}
