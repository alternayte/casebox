using System.Net;
using System.Net.Http.Json;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Proposals;
using Casebox.Server.Features.Steering;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Deedbox;
using Microsoft.Extensions.DependencyInjection;

namespace Casebox.Server.Tests.Features;

// Step 12 on the server (docs/specs/self-evolution.md), the acceptance scenario's step 6: three
// people's corrections become a pattern; a proposal's candidates are scored on the dev batch
// against the cached baseline; the best passes the held-out gate; its pull request opens exactly
// once, even when QueueBox delivers the message twice; the merge is recorded from the poll.
[Collection(GitHubPolling.Name)]
public sealed class ProposerTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private const string Repo = "github.com/acme/proposer";

    [Fact]
    public async Task A_pattern_becomes_a_gated_pull_request_that_opens_once()
    {
        var admin = await stack.ServerA.AdminAsync();
        (
            await admin.PutAsJsonAsync(
                "/api/v1/integrations/github",
                new { mode = "token", token = FakeServices.GitHubToken },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var workspace = await EvaluationTests.ApprovedCasesAsync(stack, 10, Repo);
        var devIndex = Enumerable
            .Range(0, 10)
            .ToDictionary(i => Case.IdFor(StackFixture.OrgA, $"test:{workspace}:{i}"), i => i);

        // Three people correct the agent the same way.
        var refs = await CorrectionsAsync(workspace);
        using (var scope = stack.ServerA.Services.CreateScope())
        {
            scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
            await scope.ServiceProvider.GetRequiredService<PatternScan>().RunAsync(Ct);
        }
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        var cluster = await LeaseAsync(
            worker,
            null,
            [PatternJobs.Cluster],
            j => j["payload"]!["workspace"]!.GetValue<string>() == workspace
        );
        var asked = cluster["payload"]!["corrections"]!
            .AsArray()
            .Select(c => c!["ref"]!.GetValue<string>())
            .ToList();
        Assert.Equal(refs.Order(), asked.Order());
        await CompleteAsync(
            worker,
            cluster,
            new
            {
                model = "m",
                parts = new[]
                {
                    new
                    {
                        label = "Mocks the database in integration tests",
                        summary = "The agent mocks the database where the team uses the real one.",
                        refs = asked,
                    },
                },
            }
        );
        var patterns = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/patterns?workspace={workspace}",
            Ct
        );
        var pattern = Assert.Single(patterns.GetProperty("patterns").EnumerateArray());
        Assert.Equal(
            (3, 3),
            (
                pattern.GetProperty("corrections").GetInt32(),
                pattern.GetProperty("people").GetInt32()
            )
        );
        var patternId = pattern.GetProperty("id").GetString()!;

        // Held-out cases: three are the pattern's own steering cases; the baseline fails them and
        // four others, and the candidate passes all ten.
        var heldOut = new List<string>();
        for (var i = 0; i < 10; i++)
            heldOut.Add(
                await HeldOutCaseAsync(
                    workspace,
                    i < 3 ? $"steering:{refs[i]}" : $"held:{workspace}:{i}"
                )
            );

        // The nightly baseline scores the dev cases; it fails the first four.
        var ci = await stack.ServerA.TokenClientAsync(TokenKind.Ci);
        var baseline = await CiTests.StartBaselineAsync(ci, workspace);
        await CiTests.PlayAsync(
            ci,
            baseline,
            (_, caseId) => devIndex[caseId] >= 4,
            run => run.GetProperty("status").GetString() == "done"
        );

        // The proposer: two candidates, one that helps and one that does not.
        var run = await (
            await ci.PostAsJsonAsync(
                "/api/v1/proposer/runs",
                new
                {
                    workspace,
                    spec = CiTests.Spec,
                    prices = CiTests.Prices,
                    repeats = 1,
                    budgetRuns = 24,
                    repo = Repo,
                    pattern = patternId,
                },
                Ct
            )
        ).Content.ReadFromJsonAsync<JsonElement>(Ct);
        var ciRun = run.GetProperty("ciRun").GetString()!;
        var proposalId = Assert
            .Single(run.GetProperty("started").EnumerateArray())
            .GetProperty("proposal")
            .GetString()!;
        var helps = await BlobAsync(
            ci,
            new
            {
                repo = Repo,
                files = new Dictionary<string, string>
                {
                    ["AGENTS.md"] = "## Testing\n\n- Use the real database.\n",
                },
            }
        );
        var hurts = await BlobAsync(
            ci,
            new
            {
                repo = Repo,
                files = new Dictionary<string, string> { ["CLAUDE.md"] = "Mock everything.\n" },
            }
        );
        var candidateOf = new Dictionary<string, string>();
        var done = await PlayProposerAsync(
            ci,
            ciRun,
            payload => new
            {
                baseCommit = "base0000",
                candidates = new object[]
                {
                    new
                    {
                        edits = new[]
                        {
                            new
                            {
                                op = "add_bullet",
                                file = "AGENTS.md",
                                heading = "Testing",
                                @new = "Use the real database.",
                            },
                        },
                        overrides = helps,
                        rationale = "Says what to do.",
                    },
                    new
                    {
                        edits = new[]
                        {
                            new
                            {
                                op = "write_skill",
                                file = ".claude/skills/mock/SKILL.md",
                                @new = "Mock.",
                            },
                        },
                        overrides = hurts,
                        rationale = "Wrong on purpose.",
                    },
                },
            },
            (payload, caseId, side) =>
            {
                var overrides = payload["spec"]?["overrides"]?.GetValue<string>();
                if (
                    payload["evaluationId"]!
                        .GetValue<string>()
                        .EndsWith("-gate", StringComparison.Ordinal)
                )
                    return side == "candidate" || heldOut.IndexOf(caseId) >= 7;
                return overrides == helps;
            }
        );
        var proposal = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/proposals/{proposalId}",
            Ct
        );
        var status = proposal.GetProperty("proposal").GetProperty("status").GetString();
        Assert.True(
            status is "gate_passed" or "pr_opened",
            $"The proposal is {status}: {proposal}"
        );
        Assert.All(
            proposal.GetProperty("proposal").GetProperty("checks").EnumerateArray(),
            c => Assert.True(c.GetProperty("passed").GetBoolean(), c.ToString())
        );
        var scores = proposal.GetProperty("candidates").EnumerateArray().ToList();
        Assert.True(scores[0].GetProperty("score").GetProperty("delta").GetDouble() > 0);
        Assert.True(scores[1].GetProperty("score").GetProperty("delta").GetDouble() < 0);
        Assert.Equal(0, proposal.GetProperty("proposal").GetProperty("gateIndex").GetInt32());

        // The gate's verdict left through the outbox: one pull request on a branch named after it.
        var fake = stack.Fakes.Repo("acme/proposer");
        var branch = ProposalPrEffect.Branch(proposalId);
        await Eventually(() =>
        {
            lock (fake.Pulls)
                return fake.Pulls.Values.Any(p => p.HeadRef == branch);
        });
        FakePull pull;
        lock (fake.Pulls)
            pull = fake.Pulls.Values.Single(p => p.HeadRef == branch);
        Assert.Contains("Undo: revert this pull request.", pull.Body);
        Assert.Contains("3 corrections from 3 people", pull.Body);
        Assert.DoesNotContain("person:", pull.Body);
        Assert.Single(fake.Trees);
        Assert.Equal(
            "## Testing\n\n- Use the real database.\n",
            fake.Trees[0]["tree"]![0]!["content"]!.GetValue<string>()
        );

        // QueueBox delivers the same message again: still one branch and one pull request.
        using var management = stack.ServerA.Management();
        management.DefaultRequestHeaders.Add(
            EffectEndpoints.TokenHeader,
            StackFixture.EffectsToken
        );
        using var redelivery = new HttpRequestMessage(
            HttpMethod.Post,
            "/internal/effects/proposal-pr"
        );
        redelivery.Content = JsonContent.Create(new { proposalId });
        redelivery.Headers.Add("x-deedbox-tenant-id", StackFixture.OrgA);
        (await management.SendAsync(redelivery, Ct)).EnsureSuccessStatusCode();
        lock (fake.Pulls)
            Assert.Single(fake.Pulls.Values, p => p.HeadRef == branch);
        Assert.Single(fake.Trees);
        var opened = await admin.GetFromJsonAsync<JsonElement>(
            $"/api/v1/proposals/{proposalId}",
            Ct
        );
        Assert.Equal("pr_opened", opened.GetProperty("proposal").GetProperty("status").GetString());
        Assert.Equal(
            pull.Number,
            opened.GetProperty("proposal").GetProperty("prNumber").GetInt32()
        );

        // A person merges it; the poll records the merge.
        pull.MergedAt = DateTimeOffset.UtcNow;
        pull.UpdatedAt = DateTimeOffset.UtcNow;
        await stack
            .ServerA.Services.GetRequiredService<GitHubPoller>()
            .RefreshPullAsync(StackFixture.OrgA, Repo, pull.Number, Ct);
        await Eventually(async () =>
            (await admin.GetFromJsonAsync<JsonElement>($"/api/v1/proposals/{proposalId}", Ct))
                .GetProperty("proposal")
                .GetProperty("status")
                .GetString() == "merged"
        );

        // The pattern shows its proposal, and a second search for it now would compete with nothing.
        var detail = await admin.GetFromJsonAsync<JsonElement>($"/api/v1/patterns/{patternId}", Ct);
        Assert.Equal(
            proposalId,
            Assert
                .Single(detail.GetProperty("proposals").EnumerateArray())
                .GetProperty("id")
                .GetString()
        );
        Assert.Equal(3, detail.GetProperty("cases").GetArrayLength());
        Assert.Equal("done", done.GetProperty("status").GetString());
    }

    // After 10 gate queries the held-out set rotates: its cases go to dev, and as many of the newest
    // dev cases go to held-out, so the next gate answers on cases no earlier verdict leaked.
    [Fact]
    public async Task A_spent_held_out_budget_rotates_the_cases()
    {
        var workspace = await EvaluationTests.ApprovedCasesAsync(stack, 6, Repo);
        var heldOut = new List<string>();
        for (var i = 0; i < 2; i++)
            heldOut.Add(await HeldOutCaseAsync(workspace, $"rotate:{workspace}:{i}"));

        using var scope = stack.ServerA.Services.CreateScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        var steps = scope.ServiceProvider.GetRequiredService<ProposalSteps>();

        await steps.RotateIfSpentAsync(workspace, "p", Suite.DefaultBudget, Ct);
        Assert.Equal(1, (await store.Load<Suite>(Suite.StreamId(workspace), Ct)).State.Rotation);

        for (var i = 0; i < Suite.DefaultBudget; i++)
            await store.Execute<Suite>(
                Suite.StreamId(workspace),
                s => SuiteDecider.Query(s, $"p{i}", $"e{i}", Suite.DefaultBudget),
                Ct
            );
        await steps.RotateIfSpentAsync(workspace, "p", Suite.DefaultBudget, Ct);

        var suite = (await store.Load<Suite>(Suite.StreamId(workspace), Ct)).State;
        Assert.Equal((2, 0), (suite.Rotation, suite.Queries));
        await using var db = new Npgsql.NpgsqlConnection(stack.ConnectionString);
        var splits = (
            await Dapper.SqlMapper.QueryAsync<(string Id, string Split)>(
                db,
                "SELECT id, split FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace",
                new { Org = StackFixture.OrgA, Workspace = workspace }
            )
        ).ToDictionary(r => r.Id, r => r.Split);
        Assert.All(heldOut, id => Assert.Equal("dev", splits[id]));
        Assert.Equal(2, splits.Values.Count(s => s == "held_out"));
    }

    // Three people's classified corrections in the workspace's repository; answers their refs.
    private async Task<List<string>> CorrectionsAsync(string workspace)
    {
        using var scope = stack.ServerA.Services.CreateScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        var period = Casebox.Server.Features.Capture.Identities.PeriodOf(
            Casebox.Server.Features.Orgs.PseudonymPeriod.Quarter,
            DateTimeOffset.UtcNow
        );
        for (var i = 0; i < 3; i++)
        {
            var stream = $"steering:session:claude-code:{workspace}-{i}";
            await store.Append(
                stream,
                ExpectedVersion.Any,
                [
                    new SteeringEvents.Observed(
                        "e:1",
                        Signal.FollowUp,
                        Phase.InSession,
                        DateTimeOffset.UtcNow.AddDays(-2),
                        Repo,
                        $"claude-code:{workspace}-{i}",
                        null,
                        $"person:{Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(workspace + i)))[..26]}",
                        true,
                        period,
                        "No, use the real database in the integration test.",
                        null,
                        new SteeringRefs(Seqs: [1])
                    ),
                    new SteeringEvents.Classified(
                        "e:1",
                        Intent.Correction,
                        WentWrong.BrokeConvention,
                        null,
                        Prevention.Instruction,
                        0.9,
                        "m",
                        "v1"
                    ),
                ]
            );
        }
        await using var db = new Npgsql.NpgsqlConnection(stack.ConnectionString);
        return (
            await Dapper.SqlMapper.QueryAsync<string>(
                db,
                "SELECT ref FROM casebox.steering_facts WHERE org_id = @Org AND repo = @Repo AND session_id LIKE @Prefix ORDER BY session_id",
                new
                {
                    Org = StackFixture.OrgA,
                    Repo,
                    Prefix = $"claude-code:{workspace}-%",
                }
            )
        ).ToList();
    }

    private async Task<string> HeldOutCaseAsync(string workspace, string source)
    {
        using var scope = stack.ServerA.Services.CreateScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        var id = Case.IdFor(StackFixture.OrgA, source);
        var existing = await Dapper.SqlMapper.QuerySingleAsync<string>(
            new Npgsql.NpgsqlConnection(stack.ConnectionString),
            "SELECT recipe_hash FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace LIMIT 1",
            new { Org = StackFixture.OrgA, Workspace = workspace }
        );
        await store.Append(
            Case.StreamId(id),
            ExpectedVersion.Any,
            [
                new CaseEvents.Mined(
                    CaseKind.Capability,
                    workspace,
                    source,
                    null,
                    CaseScope.Single,
                    [new CaseRepo(Repo, "base", "merged", RepoRole.Sealed)],
                    existing,
                    new string('h', 64),
                    1
                ),
                new CaseEvents.Validated(new string('o', 64), 1, 3, 30, false, 1),
                new CaseEvents.InstructionDrafted("Use the database.", "system", [], [], [], "m"),
                new CaseEvents.Approved("acct"),
                new CaseEvents.SplitAssigned(CaseSplit.HeldOut),
            ]
        );
        return id;
    }

    private static async Task<string> BlobAsync(HttpClient client, object body)
    {
        var data = JsonSerializer.SerializeToUtf8Bytes(body);
        var hash = Convert.ToHexStringLower(SHA256.HashData(data));
        using var content = new ByteArrayContent(data);
        (await client.PutAsync($"/worker/v1/blobs/{hash}", content, Ct)).EnsureSuccessStatusCode();
        return hash;
    }

    // Plays the proposer run's inline worker until the run is done: the search job gets the
    // candidates, the diet none, each run and verify by passes(payload, case, side).
    private static async Task<JsonElement> PlayProposerAsync(
        HttpClient ci,
        string ciRun,
        Func<JsonObject, object> draft,
        Func<JsonObject, string, string, bool> passes
    )
    {
        var runs = new Dictionary<string, JsonObject>();
        var deadline = DateTime.UtcNow.AddSeconds(180);
        while (true)
        {
            var view = await ci.GetFromJsonAsync<JsonElement>($"/api/v1/ci/runs/{ciRun}", Ct);
            if (view.GetProperty("status").GetString() == "done")
                return view;
            Assert.True(DateTime.UtcNow < deadline, "The proposer run did not finish: " + view);
            var lease = await ci.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "proposer-test",
                    version = "t",
                    kinds = new[] { ProposalJobs.Search, ProposalJobs.Diet, "run", "verify" },
                    scope = ciRun,
                },
                Ct
            );
            if (lease.StatusCode == HttpStatusCode.NoContent)
            {
                await Task.Delay(200, Ct);
                continue;
            }
            lease.EnsureSuccessStatusCode();
            var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
            var payload = (JsonObject)job["payload"]!;
            object result = job["kind"]!.GetValue<string>() switch
            {
                ProposalJobs.Search => draft(payload),
                ProposalJobs.Diet => new
                {
                    baseCommit = "base0000",
                    candidates = Array.Empty<object>(),
                },
                "run" => Run(payload, runs),
                _ => Verify(payload, runs, passes),
            };
            (
                await ci.PostAsJsonAsync(
                    $"/worker/v1/jobs/{job["id"]}/complete",
                    new { workerId = "proposer-test", result },
                    Ct
                )
            ).EnsureSuccessStatusCode();
        }
    }

    private static object Run(JsonObject payload, Dictionary<string, JsonObject> runs)
    {
        var key =
            payload["evaluationId"]!.GetValue<string>()
            + "/"
            + payload["runId"]!.GetValue<string>();
        runs[key] = payload;
        return new
        {
            runId = payload["runId"]!.GetValue<string>(),
            diff = new string('d', 64),
            usage = new { inputTokens = 1000, outputTokens = 200 },
            costUsd = 0.05m,
            seconds = 60.0,
            turns = 3,
            toolCalls = 5,
            model = "claude-sonnet-5-20260801",
            processChecks = new { ranTestsBeforeDone = true, editedTestAfterFailure = false },
            harnessHash = CiTests.Hash,
        };
    }

    private static object Verify(
        JsonObject payload,
        Dictionary<string, JsonObject> runs,
        Func<JsonObject, string, string, bool> passes
    )
    {
        var runId = payload["runId"]!.GetValue<string>();
        var key = payload["evaluationId"]!.GetValue<string>() + "/" + runId;
        var side = runId.Contains(":c:", StringComparison.Ordinal) ? "candidate" : "baseline";
        var passed = passes(runs[key], payload["caseId"]!.GetValue<string>(), side);
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

    private static async Task<JsonObject> LeaseAsync(
        HttpClient worker,
        string? scope,
        string[] kinds,
        Func<JsonObject, bool> ours
    )
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (true)
        {
            Assert.True(
                DateTime.UtcNow < deadline,
                "No job of the kinds " + string.Join(", ", kinds)
            );
            var lease = await worker.PostAsJsonAsync(
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "proposer-test",
                    version = "t",
                    kinds,
                    scope,
                },
                Ct
            );
            if (lease.StatusCode == HttpStatusCode.OK)
            {
                var job = (await lease.Content.ReadFromJsonAsync<JsonObject>(Ct))!;
                if (ours(job))
                    return job;
                (
                    await worker.PostAsJsonAsync(
                        $"/worker/v1/jobs/{job["id"]}/fail",
                        new
                        {
                            workerId = "proposer-test",
                            error = "not this test's",
                            retryable = true,
                        },
                        Ct
                    )
                ).EnsureSuccessStatusCode();
            }
            await Task.Delay(200, Ct);
        }
    }

    private static async Task CompleteAsync(HttpClient worker, JsonObject job, object result) =>
        (
            await worker.PostAsJsonAsync(
                $"/worker/v1/jobs/{job["id"]}/complete",
                new { workerId = "proposer-test", result },
                Ct
            )
        ).EnsureSuccessStatusCode();

    private static async Task Eventually(Func<bool> condition) =>
        await Eventually(() => Task.FromResult(condition()));

    private static async Task Eventually(Func<Task<bool>> condition)
    {
        var deadline = DateTime.UtcNow.AddSeconds(60);
        while (!await condition())
        {
            Assert.True(
                DateTime.UtcNow < deadline,
                "The condition did not hold within 60 seconds."
            );
            await Task.Delay(300, Ct);
        }
    }
}
