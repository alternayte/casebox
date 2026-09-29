using System.Security.Cryptography;
using System.Text;
using Casebox.Server.Features.Capture;
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

// `casebox up --demo`: a synthetic team of five on one repository, written through the streams and
// projections as real data would be. Every person is
// a synthetic token; no name, email or login exists anywhere. It loads only into an organisation
// with no session.
public sealed class DemoSeed(
    IEventStore store,
    NpgsqlDataSource db,
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
        var rng = new Random(20260929);
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

        await WorkspaceAsync(ct);

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
            var at = now.AddDays(-29 + i * 1.1).AddHours(rng.Next(8));
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
                        Ended = at.AddMinutes(35 + rng.Next(40)),
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

        // The patterns, and a proposal for each in a different state (docs/specs/simple-evolution.md).
        async Task<string> PatternAsync(int t, string title, string summary)
        {
            var key = new PatternKey(
                SteeringFacts.Enum(Themes[t].WentWrong),
                null,
                SteeringFacts.Enum(Themes[t].Prevention),
                Themes[t].Path
            );
            var id = Pattern.IdFor(Org, Workspace, key, refs[t]);
            await store.Execute<Pattern>(
                Pattern.StreamId(id),
                p =>
                    PatternDecider.Detect(
                        p,
                        new PatternEvents.Detected(
                            Workspace,
                            key.WentWrong,
                            null,
                            key.Prevention,
                            key.Path,
                            title,
                            summary,
                            refs[t],
                            Pattern.IsAdvisory(key.Prevention)
                        )
                    ),
                ct
            );
            return id;
        }

        var mocks = await PatternAsync(
            0,
            "Mocks the database in integration tests",
            "The agent mocks the repository in integration tests where the team uses the real database."
        );
        var untested = await PatternAsync(
            1,
            "Says done without running the tests",
            "The agent reports the change as finished before the test suite passes."
        );
        var tickets = await PatternAsync(
            2,
            "Misses acceptance criteria in the ticket",
            "The agent implements part of the ticket; the missing criteria were in its text."
        );
        await store.Execute<Pattern>(
            Pattern.StreamId(tickets),
            p =>
                PatternDecider.NoteAdvisory(
                    p,
                    "The criteria were in the tickets; a clearer ticket template helps more than a rule in the repository."
                ),
            ct
        );
        var outbox = await PatternAsync(
            3,
            "Calls the ledger directly instead of the outbox",
            "The agent calls other services synchronously where the team publishes an event through the outbox."
        );

        const string agentsMd =
            "# Payments\n\n## Testing\n\n- Run `just test` before you say a change is done.\n";
        var mocksEdit = new Proposals.Edit(
            "add_bullet",
            "AGENTS.md",
            "Testing",
            null,
            "Integration tests use the real database from the test container; never mock the repository or the DbContext."
        );
        var applied = await ProposalAsync(
            mocks,
            ProposalKind.HarnessEdit,
            "Say that integration tests use the real database",
            "Every correction in this pattern replaced a mock with the test container's database; AGENTS.md says nothing about it.",
            [mocksEdit],
            [
                new FilePreview(
                    "AGENTS.md",
                    agentsMd,
                    agentsMd
                        + "- Integration tests use the real database from the test container; never mock the repository or the DbContext.\n"
                ),
            ],
            null,
            ct
        );
        await store.Execute<Proposal>(
            Proposal.StreamId(applied),
            p => ProposalDecider.Approve(p, "system:demo"),
            ct
        );
        await store.Execute<Proposal>(
            Proposal.StreamId(applied),
            p => ProposalDecider.Apply(p, ApplyMode.Private, "system:demo", now.AddDays(-2)),
            ct
        );

        const string skill =
            "---\nname: outbox\ndescription: Publish side effects through the outbox. Use when a change calls another service, the ledger or a webhook.\n---\n\n# Outbox\n\n1. Never call another service inside a request.\n2. Append the event and its outbox message in the same transaction.\n3. Add a handler test that reads the outbox table.\n";
        await ProposalAsync(
            outbox,
            ProposalKind.Skill,
            "Add an outbox skill",
            "Three people moved a direct ledger call to the outbox; a short skill gives the agent the procedure when it touches another service.",
            [
                new Proposals.Edit(
                    "write_skill",
                    ".claude/skills/outbox/SKILL.md",
                    null,
                    null,
                    skill
                ),
            ],
            [new FilePreview(".claude/skills/outbox/SKILL.md", null, skill)],
            null,
            ct
        );

        await ProposalAsync(
            untested,
            ProposalKind.CodeNote,
            "Make the test command fail loudly before a change is done",
            "The agent said done while tests failed; the repository has no single command that runs every suite.",
            [],
            [],
            new CodeNote(
                "Add a `just test` recipe that runs the unit and integration suites and exits non-zero on any failure.",
                "Four corrections asked the agent to run the tests; today they need two commands and a running database.",
                "Add a `just test` recipe to the justfile that starts the test database, runs `dotnet test` for every test project, and exits non-zero when any test fails. Then change the Testing section of AGENTS.md to name `just test` as the one command to run before saying a change is done."
            ),
            ct
        );
        return true;
    }

    private async Task<string> ProposalAsync(
        string pattern,
        ProposalKind kind,
        string title,
        string rationale,
        IReadOnlyList<Proposals.Edit> edits,
        IReadOnlyList<FilePreview> preview,
        CodeNote? note,
        CancellationToken ct
    )
    {
        var id = Ids.New();
        await store.Execute<Proposal>(
            Proposal.StreamId(id),
            p =>
                ProposalDecider.Draft(
                    p,
                    new ProposalEvents.Drafted(
                        pattern,
                        Workspace,
                        Repo,
                        kind,
                        title,
                        rationale,
                        edits,
                        preview,
                        note,
                        ProposalJobs.ContentHash(kind, edits, note),
                        Hash("demo-head")[..40],
                        "demo"
                    )
                ),
            ct
        );
        return id;
    }

    private async Task WorkspaceAsync(CancellationToken ct)
    {
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
