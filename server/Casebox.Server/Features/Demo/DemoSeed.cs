using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Proposals;
using Casebox.Server.Features.Steering;
using Casebox.Server.Features.WorkItems;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Demo;

// `casebox up --demo` (docs/specs/operations.md, Demo data): a synthetic team of five on one
// repository, written through the streams and projections as real data would be. Every person is
// a synthetic token; no name, email or login exists anywhere. It loads only into an organisation
// with no session.
public sealed class DemoSeed(
    IEventStore store,
    NpgsqlDataSource db,
    BlobStore blobs,
    DeedboxContext context,
    TimeProvider clock
)
{
    public const string Workspace = "demo";
    public const string Repo = "github.com/acme/payments";

    private string Org => context.TenantId;

    private sealed record Theme(
        WentWrong WentWrong,
        Prevention Prevention,
        string Path,
        string[] Quotes
    );

    // What the team keeps correcting, with the words a person would type.
    private static readonly Theme[] Themes =
    [
        new(
            WentWrong.BrokeConvention,
            Prevention.Instruction,
            "api",
            [
                "No, integration tests use the real database here. Do not mock the repository.",
                "Please drop the mock, we run Postgres in the test container.",
                "Again a mocked DbContext in an integration test; use the fixture database.",
            ]
        ),
        new(
            WentWrong.UnverifiedDone,
            Prevention.Verification,
            "api",
            [
                "You said it is done but the tests fail. Run just test before you finish.",
                "Did you run the tests? Two of them are red.",
                "Please run the full suite before saying it works.",
            ]
        ),
        new(
            WentWrong.MissedRequirement,
            Prevention.ClearerTicket,
            "web",
            [
                "The ticket also asks for the refund total in the summary row.",
                "You missed the second acceptance criterion about currency rounding.",
                "It should also show the pending state, see the ticket.",
            ]
        ),
        new(
            WentWrong.WrongApproach,
            Prevention.Skill,
            "api",
            [
                "Use the outbox for the payment event, not a direct HTTP call.",
                "We never call the ledger synchronously; publish the event instead.",
                "This should go through the outbox like the other side effects.",
            ]
        ),
    ];

    public async Task<bool> SeedAsync(CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        if (
            await connection.ExecuteScalarAsync<bool>(
                new CommandDefinition(
                    "SELECT EXISTS (SELECT 1 FROM casebox.sessions WHERE org_id = @Org)",
                    new { Org },
                    cancellationToken: ct
                )
            )
        )
            return false;

        var now = clock.GetUtcNow();
        var rng = new Statistics.Rng(Statistics.SeedOf("casebox-demo"));
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        if (org.Settings.PromptMode is null)
            await store.Execute<Organisation>(
                Organisation.StreamId,
                o =>
                    OrgDecider.ChangeSettings(
                        o,
                        o.Settings with
                        {
                            PromptMode = PromptMode.Redacted,
                        }
                    ),
                ct
            );
        var period = (DateTimeOffset at) => Identities.PeriodOf(org.Settings.PseudonymPeriod, at);
        var people = Enumerable.Range(1, 5).Select(i => Token($"demo-person-{i}")).ToArray();

        var recipeHash = await WorkspaceAsync(ct);

        // 24 work items over the last 28 days, each with a session and a merged pull request.
        var agents = new[]
        {
            ("claude-code", "claude-sonnet-5-20260801"),
            ("codex", "gpt-6-sol-2026-06-01"),
        };
        var sessions = new List<(string Id, string Person, DateTimeOffset At, int Item)>();
        for (var i = 1; i <= 24; i++)
        {
            // Inside the report's default 30 days, so the first page shows the whole team.
            var at = now.AddDays(-29 + i * 1.1).AddHours(rng.NextInt(8));
            var person = people[(i - 1) % people.Length];
            var (agent, model) = agents[i % 2];
            var session = $"{agent}:demo-{i:D2}";
            sessions.Add((session, person, at, i));
            await connection.ExecuteAsync(
                new CommandDefinition(
                    """
                    INSERT INTO casebox.sessions (org_id, id, agent, agent_version, model, repo, branch, person, person_mapped, period, source, started_at,
                        ended_at, event_count, created_at, updated_at, harness_hash, harness_version, commits, task_type)
                    VALUES (@Org, @Id, @Agent, @Version, @Model, @Repo, @Branch, @Person, true, @Period, 'import', @At, @Ended, 40, @At, @At,
                        @Harness, @HarnessVersion, 2, @Task)
                    """,
                    new
                    {
                        Org,
                        Id = session,
                        Agent = agent,
                        Version = agent == "codex" ? "0.159.0" : "2.1.284",
                        Model = model,
                        Repo,
                        Branch = $"PAY-{i}-change",
                        Person = person,
                        Period = period(at),
                        At = at,
                        Ended = at.AddMinutes(35 + rng.NextInt(40)),
                        Harness = new string('a', 64),
                        HarnessVersion = Hash("demo-harness|" + agent + "|" + model),
                        Task = i % 3 == 0 ? "bug" : "feature",
                    },
                    cancellationToken: ct
                )
            );
            var item = $"wi:jira:PAY-{i}";
            var merged = at.AddHours(6);
            await store.Append(
                item,
                ExpectedVersion.Any,
                [
                    new WorkItemEvents.Imported("jira", $"PAY-{i}", Repo, at.AddDays(-1)),
                    new WorkItemEvents.SnapshotCaptured(
                        new WorkItems.Snapshot(
                            $"Payments change {i}",
                            "Synthetic demo ticket.",
                            i % 3 == 0 ? "Bug" : "Story",
                            [],
                            merged < now ? "Done" : "In Progress",
                            null,
                            merged < now,
                            Hash($"PAY-{i}")
                        ),
                        SnapshotReason.FirstLinked
                    ),
                    new WorkItemEvents.SessionLinked(session, LinkSource.Branch, Confidence.High),
                    new WorkItemEvents.PrLinked(Repo, 100 + i, LinkSource.Branch, Confidence.High),
                    new WorkItemEvents.Merged(Repo, 100 + i, Hash($"merge-{i}")[..40], merged),
                ],
                ct
            );
        }

        // Corrections: each theme from at least three people, in session; the rest of the items stay clean.
        var refs = new Dictionary<int, List<string>>();
        var n = 0;
        for (var t = 0; t < Themes.Length; t++)
        {
            refs[t] = [];
            for (var q = 0; q < 4; q++)
            {
                var s = sessions[(t * 5 + q * 2) % sessions.Count];
                var person = people[(t + q) % people.Length];
                var stream = $"steering:session:{s.Id}";
                var id = $"e:{10 + n++}";
                var at = s.At.AddMinutes(12 + q);
                await store.Append(
                    stream,
                    ExpectedVersion.Any,
                    [
                        new SteeringEvents.Observed(
                            id,
                            Signal.FollowUp,
                            Phase.InSession,
                            at,
                            Repo,
                            s.Id,
                            null,
                            person,
                            true,
                            period(at),
                            Themes[t].Quotes[q % Themes[t].Quotes.Length],
                            null,
                            new SteeringRefs(Seqs: [10 + q])
                        ),
                        new SteeringEvents.Classified(
                            id,
                            Intent.Correction,
                            Themes[t].WentWrong,
                            null,
                            Themes[t].Prevention,
                            0.86,
                            "demo",
                            "v1"
                        ),
                    ],
                    ct
                );
                refs[t].Add(await RefAsync(connection, stream, id, ct));
                await connection.ExecuteAsync(
                    new CommandDefinition(
                        "INSERT INTO casebox.correction_paths (org_id, stream_id, intervention_id, path) VALUES (@Org, @Stream, @Id, @Path) ON CONFLICT DO NOTHING",
                        new
                        {
                            Org,
                            Stream = stream,
                            Id = id,
                            Themes[t].Path,
                        },
                        cancellationToken: ct
                    )
                );
            }
        }
        // Directions and clarifications are interventions but never corrections.
        for (var d = 0; d < 6; d++)
        {
            var s = sessions[(d * 3 + 1) % sessions.Count];
            var id = $"e:{80 + d}";
            await store.Append(
                $"steering:session:{s.Id}",
                ExpectedVersion.Any,
                [
                    new SteeringEvents.Observed(
                        id,
                        Signal.FollowUp,
                        Phase.InSession,
                        s.At.AddMinutes(25),
                        Repo,
                        s.Id,
                        null,
                        s.Person,
                        true,
                        period(s.At),
                        d % 2 == 0 ? "Also add a changelog entry." : "Yes, the EUR account.",
                        null,
                        new SteeringRefs(Seqs: [30])
                    ),
                    new SteeringEvents.Classified(
                        id,
                        d % 2 == 0 ? Intent.Direction : Intent.Clarification,
                        null,
                        null,
                        null,
                        0.9,
                        "demo",
                        "v1"
                    ),
                ],
                ct
            );
        }

        // Cases: 14 dev and 10 held-out; four of the held-out ones come from the outbox corrections.
        var dev = new List<string>();
        var heldOut = new List<string>();
        for (var c = 0; c < 24; c++)
        {
            var own = c >= 20;
            var source = own ? $"steering:{refs[0][c - 20]}" : $"pr:{Repo}#{101 + c}";
            var id = Case.IdFor(Org, source);
            var split = c < 14 ? CaseSplit.Dev : CaseSplit.HeldOut;
            (split == CaseSplit.Dev ? dev : heldOut).Add(id);
            await store.Append(
                Case.StreamId(id),
                ExpectedVersion.Any,
                [
                    new CaseEvents.Mined(
                        own ? CaseKind.Steering : CaseKind.Capability,
                        Workspace,
                        source,
                        own ? null : $"wi:jira:PAY-{c + 1}",
                        CaseScope.Single,
                        [
                            new CaseRepo(
                                Repo,
                                Hash($"base-{c}")[..40],
                                Hash($"merged-{c}")[..40],
                                RepoRole.Sealed
                            ),
                        ],
                        recipeHash,
                        new string('a', 64),
                        c
                    ),
                    new CaseEvents.Validated(
                        Hash($"oracle-{c}"),
                        1 + c % 3,
                        12 + c,
                        40 + c * 3,
                        false,
                        1
                    ),
                    new CaseEvents.InstructionDrafted(
                        $"Payments change {c + 1}: implement the behaviour the ticket describes, with tests.",
                        "system",
                        [],
                        [],
                        [],
                        "demo"
                    ),
                    new CaseEvents.Approved("system:demo"),
                    new CaseEvents.SplitAssigned(split),
                ],
                ct
            );
        }

        var baseline = new HarnessSpec(
            "claude-code",
            "2.1.284",
            "claude-sonnet-5-20260801",
            null,
            "HEAD",
            new AgentSettings(null, 30, null),
            null
        );
        var prices = new Dictionary<string, Price>
        {
            ["claude-sonnet-5-20260801"] = new(3m, 15m, 0.3m, null),
        };

        // Your harness versus none: the harness passes more often, at the same cost.
        var versusNone = Ids.New();
        await EvaluationAsync(
            connection,
            versusNone,
            Purpose.HarnessVsNone,
            "dev",
            dev,
            baseline,
            baseline with
            {
                Harness = "none",
            },
            prices,
            3,
            (c, side) => side == Side.Baseline ? 0.72 : 0.46,
            rng,
            now.AddDays(-9),
            ct
        );

        // The pattern of the outbox corrections, and its proposal through the gate to a pull request.
        var pattern = Pattern.IdFor(
            Org,
            Workspace,
            new PatternKey("broke_convention", null, "instruction", "api"),
            refs[0]
        );
        await store.Execute<Pattern>(
            Pattern.StreamId(pattern),
            p =>
                PatternDecider.Detect(
                    p,
                    new PatternEvents.Detected(
                        Workspace,
                        "broke_convention",
                        null,
                        "instruction",
                        "api",
                        "Mocks the database in integration tests",
                        "The agent mocks the repository in integration tests where the team uses the real database.",
                        refs[0],
                        false
                    )
                ),
            ct
        );
        foreach (var (t, advisory) in new[] { (1, false), (2, true) })
        {
            var key = new PatternKey(
                SteeringFacts.Enum(Themes[t].WentWrong),
                null,
                SteeringFacts.Enum(Themes[t].Prevention),
                Themes[t].Path
            );
            await store.Execute<Pattern>(
                Pattern.StreamId(Pattern.IdFor(Org, Workspace, key, refs[t])),
                p =>
                    PatternDecider.Detect(
                        p,
                        new PatternEvents.Detected(
                            Workspace,
                            key.WentWrong,
                            null,
                            key.Prevention,
                            key.Path,
                            t == 1
                                ? "Says done without running the tests"
                                : "Misses acceptance criteria in the ticket",
                            t == 1
                                ? "The agent reports the change as finished before the test suite passes."
                                : "The agent implements part of the ticket; the missing criteria were in its text.",
                            refs[t],
                            advisory
                        )
                    ),
                ct
            );
        }

        var overrides = JsonSerializer.SerializeToUtf8Bytes(
            new
            {
                repo = Repo,
                files = new Dictionary<string, string>
                {
                    ["AGENTS.md"] =
                        "# Payments\n\n## Testing\n\n- Run just test before you say a change is done.\n- Integration tests use the real database; do not mock it.\n",
                },
            }
        );
        var overridesHash = BlobStore.HashOf(overrides);
        await blobs.PutAsync(Org, overridesHash, "application/json", overrides, ct);
        var proposal = Ids.New();
        var edit = new Edit(
            "add_bullet",
            "AGENTS.md",
            "Testing",
            null,
            "Integration tests use the real database; do not mock it."
        );
        var candidate = new Proposals.Candidate(
            0,
            [edit],
            overridesHash,
            ProposalSteps.ContentHash([edit]),
            "Names the team's rule where the agent looks for test conventions."
        );
        var gate = ProposalSteps.GateId(proposal);
        await EvaluationAsync(
            connection,
            gate,
            Purpose.Gate,
            "held_out",
            heldOut,
            baseline,
            baseline with
            {
                Overrides = overridesHash,
            },
            prices,
            3,
            (c, side) => heldOut.IndexOf(c) >= 6 ? (side == Side.Candidate ? 0.9 : 0.25) : 0.75,
            rng,
            now.AddDays(-4),
            ct,
            proposal,
            0
        );
        var checks = new List<GateCheck>
        {
            new("quality", true, "strong", "Verdict better on 10 held-out cases."),
            new(
                "pattern",
                true,
                "strong",
                "The pattern's 4 held-out cases: candidate 90% passed, baseline 25%."
            ),
            new(
                "process",
                true,
                "strong",
                "Ran the tests before saying done: candidate 83%, baseline 80%. Edited a test after a failure: 0% on both."
            ),
        };
        await store.Append(
            Proposal.StreamId(proposal),
            ExpectedVersion.Any,
            [
                new ProposalEvents.Drafted(
                    pattern,
                    Workspace,
                    ProposalKind.Edit,
                    Repo,
                    Hash("demo-main")[..40],
                    [candidate],
                    baseline,
                    prices,
                    3,
                    40,
                    dev.Take(8).ToList()
                ),
                new ProposalEvents.CandidateScored(
                    0,
                    $"{proposal}-c0",
                    0.25,
                    0.05,
                    0.45,
                    dev.Take(2).ToList(),
                    0.97,
                    8
                ),
                new ProposalEvents.GateRequested(0, gate),
                new ProposalEvents.GatePassed(gate, checks),
                new ProposalEvents.PrOpened(
                    Repo,
                    142,
                    $"casebox/proposal-{proposal.ToLowerInvariant()}",
                    $"https://{Repo}/pull/142"
                ),
            ],
            ct
        );
        await store.Execute<Suite>(
            Suite.StreamId(Workspace),
            s => SuiteDecider.Query(s, proposal, gate, Suite.DefaultBudget),
            ct
        );
        return true;
    }

    private async Task<string> WorkspaceAsync(CancellationToken ct)
    {
        const string recipe =
            """{"image":"mcr.microsoft.com/dotnet/sdk:10.0","install":["dotnet restore"],"lockfiles":[],"test":[{"command":"dotnet test --logger trx --results-directory /results","results":"trx"}],"services":{"postgres":{"image":"postgres:16","env":{"POSTGRES_PASSWORD":"test"}}},"links":[]}""";
        var hash = Hash(recipe);
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w => WorkspaceDecider.Create(w, Workspace),
            ct
        );
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w => WorkspaceDecider.AddRepo(w, Repo),
            ct
        );
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w =>
                WorkspaceDecider.ConfigureHarness(
                    w,
                    ["AGENTS.md", "CLAUDE.md", ".claude/skills/**"],
                    null
                ),
            ct
        );
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w => WorkspaceDecider.ProposeRecipe(w, recipe, hash),
            ct
        );
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w => WorkspaceDecider.RecordValidation(w, hash, true, null),
            ct
        );
        await store.Execute<Workspaces.Workspace>(
            Workspaces.Workspace.StreamIdFor(Workspace),
            w => WorkspaceDecider.ConfirmRecipe(w, hash),
            ct
        );
        return hash;
    }

    // One evaluation, written whole: its runs, its checkpoints and its verdict, with the numbers the
    // statistics engine gives for those runs.
    private async Task EvaluationAsync(
        System.Data.Common.DbConnection connection,
        string id,
        Purpose purpose,
        string split,
        IReadOnlyList<string> cases,
        HarnessSpec baseline,
        HarnessSpec candidate,
        Dictionary<string, Price> prices,
        int repeats,
        Func<string, Side, double> passRate,
        Statistics.Rng rng,
        DateTimeOffset at,
        CancellationToken ct,
        string? proposal = null,
        int? candidateIndex = null
    )
    {
        var events = new List<object>();
        var perRun = 0.62m;
        var estimate = new Estimate(
            cases.Count,
            cases.Count * 2 * repeats,
            cases.Count * repeats * 400_000L,
            cases.Count * repeats * 380_000L,
            perRun * cases.Count * repeats,
            perRun * cases.Count * repeats,
            2 * perRun * cases.Count * repeats,
            cases.Count * repeats * 2 * 14,
            14 * repeats,
            cases.Count * repeats * 2 * 14,
            2 * perRun * cases.Count,
            Statistics.DetectableEffect(cases.Count, repeats)
        );
        events.Add(
            new EvaluationEvents.Requested(
                Workspace,
                split,
                [.. cases.Select(c => new EvaluationCase(c, 1))],
                baseline,
                candidate,
                "harness",
                repeats,
                0.05,
                150m,
                estimate,
                purpose,
                false,
                false,
                prices,
                null,
                null,
                proposal,
                candidateIndex
            )
        );
        var runs = new List<Statistics.CaseRuns>();
        var results = new List<object>();
        foreach (var c in cases)
        {
            var b = new List<bool>();
            var k = new List<bool>();
            var bc = new List<double>();
            var kc = new List<double>();
            var bs = new List<double>();
            var ks = new List<double>();
            for (var r = 1; r <= repeats; r++)
                foreach (var side in new[] { Side.Baseline, Side.Candidate })
                {
                    var passed = rng.NextDouble() < passRate(c, side);
                    var cost = Math.Round(0.45m + (decimal)rng.NextDouble() * 0.35m, 4);
                    var seconds = 480 + rng.NextInt(600);
                    var runId = Evaluation.RunId(c, side, r);
                    events.Add(
                        new EvaluationEvents.RunCompleted(
                            runId,
                            c,
                            side,
                            r,
                            passed,
                            cost,
                            seconds,
                            380_000,
                            "strong"
                        )
                    );
                    (side == Side.Baseline ? b : k).Add(passed);
                    (side == Side.Baseline ? bc : kc).Add((double)cost);
                    (side == Side.Baseline ? bs : ks).Add(seconds);
                    results.Add(
                        new
                        {
                            Org,
                            Evaluation = id,
                            Run = runId,
                            Case = c,
                            Side = side == Side.Baseline ? "baseline" : "candidate",
                            Repeat = r,
                            Passed = passed,
                            Cost = cost,
                            Seconds = (double)seconds,
                            At = at,
                        }
                    );
                }
            runs.Add(new Statistics.CaseRuns(c, 1, b, k, bc, kc, bs, ks));
        }
        var level = Statistics.Level(repeats, repeats);
        var v = Statistics.Evaluate(runs, level, 0.05, Statistics.Resamples, Statistics.SeedOf(id));
        for (var round = 1; round <= repeats; round++)
            events.Add(
                new EvaluationEvents.CheckpointEvaluated(
                    round,
                    Statistics.Level(round, repeats),
                    v.Cases,
                    v.Delta,
                    v.Lower,
                    v.Upper,
                    round == repeats ? v.Verdict : Statistics.Verdict.Inconclusive
                )
            );
        events.Add(
            new EvaluationEvents.VerdictReached(
                v.Verdict,
                v.Delta,
                v.Lower,
                v.Upper,
                level,
                v.Cases,
                runs.Sum(r => r.Baseline.Count + r.Candidate.Count),
                v.CostRatio,
                v.CostLower,
                v.CostUpper,
                v.DurationRatio,
                v.DurationLower,
                v.DurationUpper,
                v.EquivalentAndCheaper,
                v.BaselineRate,
                v.CandidateRate,
                Statistics.InconclusiveReason(v.Verdict, v.Lower, v.Upper, 0.05, v.Cases),
                purpose
            )
        );
        await store.Append(Evaluation.StreamId(id), ExpectedVersion.Any, events, ct);
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.run_results (org_id, evaluation_id, run_id, case_id, side, repeat, status, passed, applied, cost_usd, seconds, model,
                    usage, created_at, completed_at)
                VALUES (@Org, @Evaluation, @Run, @Case, @Side, @Repeat, 'completed', @Passed, true, @Cost, @Seconds, 'claude-sonnet-5-20260801',
                    '{"inputTokens":140000,"outputTokens":20000,"cacheReadTokens":220000}', @At, @At)
                ON CONFLICT DO NOTHING
                """,
                results,
                cancellationToken: ct
            )
        );
    }

    private Task<string> RefAsync(
        System.Data.Common.DbConnection connection,
        string stream,
        string id,
        CancellationToken ct
    ) =>
        connection.QuerySingleAsync<string>(
            new CommandDefinition(
                "SELECT ref FROM casebox.steering_facts WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = @Id",
                new
                {
                    Org,
                    Stream = stream,
                    Id = id,
                },
                cancellationToken: ct
            )
        );

    // A synthetic person token in the pseudonymizer's shape; it never came from an identity.
    private static string Token(string seed) => "person:" + Hash(seed)[..26];

    private static string Hash(string text) =>
        Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(text)));
}
