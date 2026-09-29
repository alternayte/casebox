using System.Text.Json;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Steering;

public enum Dimension
{
    Workspace,
    Repo,
    Agent,
    Model,
    Harness,
    TaskType,
}

public sealed record ReportQuery(
    DateTimeOffset From,
    DateTimeOffset To,
    string? Workspace,
    string? Repo,
    string? Agent,
    string? Model,
    string? Harness,
    string? TaskType,
    Dimension? GroupBy
)
{
    public string? Filter(Dimension d) =>
        d switch
        {
            Dimension.Workspace => Workspace,
            Dimension.Repo => Repo,
            Dimension.Agent => Agent,
            Dimension.Model => Model,
            Dimension.Harness => Harness,
            _ => TaskType,
        };
}

public static class ReportShapes
{
    public sealed record Period(DateTimeOffset From, DateTimeOffset To);

    public sealed record Rate(double Value, int N, double[] Interval);

    public sealed record Spread(double Median, double P75);

    public sealed record PerItem(Spread? InSession, Spread? BeforeMerge, Spread? AfterMerge, int N);

    public sealed record AutonomousRun(Spread? Turns, Spread? ToolCalls, int N, double Uncorrected);

    public sealed record Headline(
        Rate? CorrectionFreeRate,
        PerItem? CorrectionsPerWorkItem,
        AutonomousRun? AutonomousRun,
        Rate? AfterMergeRate,
        Rate? AbandonmentRate
    );

    public sealed record Quote(string Ref, string Text, string Day);

    public sealed record PreventionShare(string Prevention, double Share);

    public sealed record Phases(int InSession, int BeforeMerge, int AfterMerge);

    public sealed record Theme(
        string WentWrong,
        string? Label,
        int Corrections,
        int People,
        bool HarnessFixable,
        IReadOnlyList<PreventionShare> Prevention,
        Phases Phases,
        IReadOnlyList<Quote> Quotes,
        // The patterns of this what-went-wrong class (docs/specs/self-evolution.md).
        IReadOnlyList<ThemePattern>? Patterns = null
    );

    public sealed record ThemePattern(string Id, string Title, string Status, bool Advisory);

    public sealed record MixEntry(
        string Prevention,
        double Share,
        int Corrections,
        bool HarnessFixable
    );

    public sealed record AfterMerge(int Reverts, int Fixes, int People);

    public sealed record Group(
        string Key,
        int People,
        int Sessions,
        Rate? CorrectionFreeRate,
        Rate? AbandonmentRate,
        Rate? AfterMergeRate,
        int Corrections
    );

    public sealed record Hidden(int Themes, int Groups, int Prevention, int Headline);

    public sealed record AgentCoverage(string Agent, int Sessions, IReadOnlyList<string> Sources);

    public sealed record Coverage(
        int Sessions,
        int People,
        int UnmappedSessions,
        IReadOnlyList<AgentCoverage> Agents,
        string? PromptMode,
        int Interventions,
        int Pending,
        int Unclassified,
        bool WorkerSeen
    );

    // Solo: the organisation's only person sees their own data without k (Privacy/Solo.cs).
    public sealed record Report(
        Period Period,
        int K,
        bool Solo,
        Coverage Coverage,
        Headline Headline,
        IReadOnlyList<Theme> Themes,
        IReadOnlyList<MixEntry> PreventionMix,
        AfterMerge? AfterMerge,
        IReadOnlyList<Group> Groups,
        Hidden Hidden
    );
}

// The steering report (SDD section 6, docs/specs/steering.md "Report"). Every metric is
// observational. Every shown number has at least k distinct mapped people behind it; everything
// else is null or counted as hidden.
public sealed class SteeringReports(
    NpgsqlDataSource db,
    DeedboxContext context,
    IEventStore store,
    Solo solo
)
{
    private static readonly HashSet<string> HarnessFixable =
    [
        "instruction",
        "skill",
        "verification",
    ];

    private string Org => context.TenantId;

    // What a unit of data (a session, an intervention, a work item, a pull request) belongs to, per dimension.
    private sealed record Dims(IReadOnlyDictionary<Dimension, IReadOnlySet<string>> Values)
    {
        public IReadOnlySet<string> this[Dimension d] => Values[d];

        public bool Pass(ReportQuery q) =>
            Enum.GetValues<Dimension>().All(d => q.Filter(d) is not { } f || Values[d].Contains(f));

        public static Dims Union(IEnumerable<Dims> all)
        {
            var list = all.ToList();
            return new Dims(
                Enum.GetValues<Dimension>()
                    .ToDictionary(
                        d => d,
                        d =>
                            (IReadOnlySet<string>)
                                list.SelectMany(x => x[d]).ToHashSet(StringComparer.Ordinal)
                    )
            );
        }
    }

    private sealed class Context(ILookup<string, string> workspaces)
    {
        public Dims Of(
            string? repo,
            string? agent,
            string? model,
            string? harness,
            string? taskType
        ) =>
            new(
                new Dictionary<Dimension, IReadOnlySet<string>>
                {
                    [Dimension.Workspace] = (repo is null ? [] : workspaces[repo]).ToHashSet(
                        StringComparer.Ordinal
                    ),
                    [Dimension.Repo] = One(repo),
                    [Dimension.Agent] = One(agent),
                    [Dimension.Model] = One(model),
                    [Dimension.Harness] = One(harness),
                    [Dimension.TaskType] = One(taskType),
                }
            );

        private static HashSet<string> One(string? value) => [value ?? "unknown"];
    }

    private sealed record SessionRow(
        string Id,
        string Agent,
        string? Model,
        string Repo,
        string? HarnessVersion,
        string Person,
        bool PersonMapped,
        string Period,
        string Source,
        string? TaskType,
        int? Commits,
        DateTime? EndedAt,
        string? ItemType,
        string? ItemLabels,
        bool Abandoned,
        bool Active,
        bool Uncertain,
        DateTime? FirstCorrection,
        int Turns,
        int Tools
    );

    private sealed record FactRow(
        string Ref,
        string Signal,
        string Phase,
        DateTime At,
        string Repo,
        string? SessionId,
        int? Number,
        string Person,
        bool PersonMapped,
        string Period,
        string? Text,
        string Status,
        string? UnclassifiedReason,
        string? Intent,
        string? WentWrong,
        string? WentWrongLabel,
        string? Prevention,
        double? Confidence,
        string? LabelSource,
        string? Agent,
        string? Model,
        string? Harness,
        string? SessionTask,
        string? ItemType,
        string? ItemLabels
    );

    private sealed record ItemSession(
        string WorkItemId,
        string Agent,
        string? Model,
        string Repo,
        string? HarnessVersion,
        string? TaskType,
        string Person,
        bool PersonMapped,
        string Period
    );

    private sealed record ItemCorrection(
        string WorkItemId,
        string Phase,
        string Person,
        bool PersonMapped,
        string Period
    );

    private sealed record PrRow(
        string Repo,
        int Number,
        string? WorkItemId,
        string? Author,
        bool AuthorMapped,
        bool AuthorBot,
        DateTime CreatedAt,
        string? Agent,
        string? Model,
        string? Harness,
        string? SessionTask,
        string? ItemType,
        string? ItemLabels,
        bool Corrected
    );

    private sealed record Item(
        Dims Dims,
        IReadOnlyList<Person> People,
        int InSession,
        int BeforeMerge,
        int AfterMerge
    )
    {
        public bool Clean => InSession + BeforeMerge + AfterMerge == 0;
    }

    public async Task<ReportShapes.Report> BuildAsync(ReportQuery q, CancellationToken ct)
    {
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var k = await solo.ViewAsync(org, ct);
        await using var connection = await db.OpenConnectionAsync(ct);
        var args = new
        {
            Org,
            q.From,
            q.To,
            Otel = InSession.OtelSeqBase,
            Entire = InSession.EntireSeqBase,
        };

        var workspaces = (
            await connection.QueryAsync<(string Name, string Repos)>(
                new CommandDefinition(
                    "SELECT name, repos::text FROM casebox.workspaces WHERE org_id = @Org",
                    args,
                    cancellationToken: ct
                )
            )
        )
            .SelectMany(w =>
                JsonSerializer.Deserialize<string[]>(w.Repos)!.Select(r => (Repo: r, w.Name))
            )
            .ToLookup(x => x.Repo, x => x.Name, StringComparer.Ordinal);
        var dims = new Context(workspaces);

        var sessions = (
            await connection.QueryAsync<SessionRow>(
                new CommandDefinition(
                    """
                    SELECT s.id, s.agent, s.model, s.repo, s.harness_version, s.person, s.person_mapped, s.period, s.source, s.task_type, s.commits, s.ended_at,
                           w.type AS item_type, (w.snapshot->'labels')::text AS item_labels,
                           EXISTS (SELECT 1 FROM casebox.steering_facts f WHERE f.org_id = s.org_id AND f.session_id = s.id AND f.signal = 'abandoned') AS abandoned,
                           EXISTS (SELECT 1 FROM casebox.session_events e WHERE e.org_id = s.org_id AND e.session_id = s.id AND e.kind IN ('response', 'tool_call')) AS active,
                           coalesce(run.uncertain, false) AS uncertain, run.first_correction,
                           (SELECT count(*) FROM casebox.session_events e WHERE e.org_id = s.org_id AND e.session_id = s.id AND e.kind = 'response'
                              AND (e.seq < @Otel OR e.seq >= @Entire) AND (run.first_correction IS NULL OR e.at < run.first_correction))::int AS turns,
                           (SELECT count(*) FROM casebox.session_events e WHERE e.org_id = s.org_id AND e.session_id = s.id AND e.kind = 'tool_call'
                              AND (e.seq < @Otel OR e.seq >= @Entire) AND (run.first_correction IS NULL OR e.at < run.first_correction))::int AS tools
                    FROM casebox.sessions s
                    LEFT JOIN casebox.work_items w ON w.org_id = s.org_id AND w.id = s.work_item_id
                    LEFT JOIN LATERAL (
                        SELECT min(f.at) FILTER (WHERE f.intent = 'correction') AS first_correction,
                               bool_or(f.signal <> 'abandoned' AND (f.status = 'pending' OR f.status = 'unclassified')) AS uncertain
                        FROM casebox.steering_facts f WHERE f.org_id = s.org_id AND f.session_id = s.id AND f.phase = 'in_session') run ON true
                    WHERE s.org_id = @Org AND s.repo IS NOT NULL AND s.started_at >= @From AND s.started_at < @To
                    """,
                    args,
                    cancellationToken: ct
                )
            )
        ).Select(s => (Row: s, Dims: dims.Of(s.Repo, s.Agent, s.Model, s.HarnessVersion, TaskTypes.Of(s.ItemType, s.ItemLabels) ?? s.TaskType))).Where(s => s.Dims.Pass(q)).ToList();

        var facts = (
            await connection.QueryAsync<FactRow>(
                new CommandDefinition(
                    """
                    SELECT f.ref, f.signal, f.phase, f.at, f.repo, f.session_id, f.number, f.person, f.person_mapped, f.period, f.text, f.status, f.unclassified_reason,
                           f.intent, f.went_wrong, f.went_wrong_label, f.prevention, f.confidence, f.label_source,
                           coalesce(s.agent, ps.agent) AS agent, coalesce(s.model, ps.model) AS model, coalesce(s.harness_version, ps.harness_version) AS harness,
                           coalesce(s.task_type, ps.task_type) AS session_task, w.type AS item_type, (w.snapshot->'labels')::text AS item_labels
                    FROM casebox.steering_facts f
                    LEFT JOIN casebox.sessions s ON s.org_id = f.org_id AND s.id = f.session_id
                    LEFT JOIN casebox.pull_requests p ON f.session_id IS NULL AND p.org_id = f.org_id AND p.repo = f.repo AND p.number = f.number
                    LEFT JOIN LATERAL (SELECT x.agent, x.model, x.harness_version, x.task_type FROM casebox.sessions x
                                       WHERE x.org_id = p.org_id AND x.repo = p.repo AND x.branch = p.head_ref ORDER BY x.started_at LIMIT 1) ps ON true
                    LEFT JOIN casebox.work_items w ON w.org_id = f.org_id AND w.id = coalesce(s.work_item_id, p.work_item_id)
                    WHERE f.org_id = @Org AND f.at >= @From AND f.at < @To
                    """,
                    args,
                    cancellationToken: ct
                )
            )
        ).Select(f => (Row: f, Dims: dims.Of(f.Repo, f.Agent, f.Model, f.Harness, TaskTypes.Of(f.ItemType, f.ItemLabels) ?? f.SessionTask))).Where(f => f.Dims.Pass(q)).ToList();

        var items = (await ItemsAsync(connection, org.Settings, dims, args, ct))
            .Where(i => i.Dims.Pass(q))
            .ToList();

        var prs = (
            await connection.QueryAsync<PrRow>(
                new CommandDefinition(
                    """
                    SELECT p.repo, p.number, p.work_item_id, p.snapshot->'author'->>'token' AS author, coalesce((p.snapshot->'author'->>'mapped')::boolean, false) AS author_mapped,
                           coalesce((p.snapshot->'author'->>'bot')::boolean, false) AS author_bot, (p.snapshot->>'createdAt')::timestamptz AS created_at,
                           ps.agent, ps.model, ps.harness_version AS harness, ps.task_type AS session_task, w.type AS item_type, (w.snapshot->'labels')::text AS item_labels,
                           EXISTS (SELECT 1 FROM casebox.steering_facts f WHERE f.org_id = p.org_id AND f.repo = p.repo AND f.number = p.number AND f.phase = 'after_merge') AS corrected
                    FROM casebox.pull_requests p
                    LEFT JOIN LATERAL (SELECT x.agent, x.model, x.harness_version, x.task_type FROM casebox.sessions x
                                       WHERE x.org_id = p.org_id AND x.repo = p.repo AND x.branch = p.head_ref ORDER BY x.started_at LIMIT 1) ps ON true
                    LEFT JOIN casebox.work_items w ON w.org_id = p.org_id AND w.id = p.work_item_id
                    WHERE p.org_id = @Org AND p.is_agent AND p.merged_at >= @From AND p.merged_at < @To
                    """,
                    args,
                    cancellationToken: ct
                )
            )
        ).Select(p => (Row: p, Dims: dims.Of(p.Repo, p.Agent, p.Model, p.Harness, TaskTypes.Of(p.ItemType, p.ItemLabels) ?? p.SessionTask), Person: p.Author is null || p.AuthorBot ? (Person?)null : new Person(p.Author, p.AuthorMapped, Identities.PeriodOf(org.Settings.PseudonymPeriod, p.CreatedAt)))).Where(p => p.Dims.Pass(q)).ToList();

        var hiddenHeadline = 0;
        T? Gate<T>(T? value, IEnumerable<Person> people)
            where T : class
        {
            if (value is null)
                return null;
            if (KRule.Meets(people, k))
                return value;
            hiddenHeadline++;
            return null;
        }

        var sessionPeople = sessions
            .Select(s => new Person(s.Row.Person, s.Row.PersonMapped, s.Row.Period))
            .ToList();
        var headline = new ReportShapes.Headline(
            Gate(CorrectionFree(items), items.SelectMany(i => i.People)),
            Gate(PerItem(items), items.SelectMany(i => i.People)),
            Gate(
                Autonomous(sessions.Select(s => s.Row)),
                sessions
                    .Where(s => s.Row.Active && !s.Row.Uncertain)
                    .Select(s => new Person(s.Row.Person, s.Row.PersonMapped, s.Row.Period))
            ),
            Gate(
                AfterMergeRate(prs.Select(p => p.Row)),
                prs.Where(p => p.Person is not null).Select(p => p.Person!.Value)
            ),
            Gate(
                Abandonment(sessions.Select(s => s.Row)),
                sessions
                    .Where(s => Counted(s.Row))
                    .Select(s => new Person(s.Row.Person, s.Row.PersonMapped, s.Row.Period))
            )
        );

        var corrections = facts.Where(f => f.Row.Intent == "correction").ToList();
        var (themes, hiddenThemes) = Themes(
            corrections.Where(f => f.Row.WentWrong is not null).Select(f => f.Row).ToList(),
            k
        );
        var (mix, hiddenMix) = PreventionMix(
            corrections.Where(f => f.Row.Prevention is not null).Select(f => f.Row).ToList(),
            k
        );

        var afterMergeFacts = corrections
            .Where(f => f.Row.Phase == "after_merge")
            .Select(f => f.Row)
            .ToList();
        var afterMergePeople = afterMergeFacts.Select(PersonOf).ToList();
        var afterMerge =
            afterMergeFacts.Count > 0 && KRule.Meets(afterMergePeople, k)
                ? new ReportShapes.AfterMerge(
                    afterMergeFacts.Count(f => f.Signal == "revert"),
                    afterMergeFacts.Count(f => f.Signal == "fix"),
                    KRule.People(afterMergePeople, k)
                )
                : null;

        var groups = new List<ReportShapes.Group>();
        var hiddenGroups = 0;
        if (q.GroupBy is { } by)
        {
            var keys = sessions
                .SelectMany(s => s.Dims[by])
                .Concat(facts.SelectMany(f => f.Dims[by]))
                .Concat(items.SelectMany(i => i.Dims[by]))
                .Concat(prs.SelectMany(p => p.Dims[by]))
                .Distinct(StringComparer.Ordinal)
                .Order(StringComparer.Ordinal);
            foreach (var key in keys)
            {
                var gSessions = sessions.Where(s => s.Dims[by].Contains(key)).ToList();
                var gFacts = facts.Where(f => f.Dims[by].Contains(key)).ToList();
                var people = gSessions
                    .Select(s => new Person(s.Row.Person, s.Row.PersonMapped, s.Row.Period))
                    .Concat(gFacts.Select(f => PersonOf(f.Row)))
                    .ToList();
                if (!KRule.Meets(people, k))
                {
                    hiddenGroups++;
                    continue;
                }

                var gItems = items.Where(i => i.Dims[by].Contains(key)).ToList();
                var gPrs = prs.Where(p => p.Dims[by].Contains(key)).ToList();
                groups.Add(
                    new ReportShapes.Group(
                        key,
                        KRule.People(people, k),
                        gSessions.Count,
                        KRule.Meets(gItems.SelectMany(i => i.People), k)
                            ? CorrectionFree(gItems)
                            : null,
                        KRule.Meets(
                            gSessions
                                .Where(s => Counted(s.Row))
                                .Select(s => new Person(
                                    s.Row.Person,
                                    s.Row.PersonMapped,
                                    s.Row.Period
                                )),
                            k
                        )
                            ? Abandonment(gSessions.Select(s => s.Row))
                            : null,
                        KRule.Meets(
                            gPrs.Where(p => p.Person is not null).Select(p => p.Person!.Value),
                            k
                        )
                            ? AfterMergeRate(gPrs.Select(p => p.Row))
                            : null,
                        gFacts.Count(f => f.Row.Intent == "correction")
                    )
                );
            }
        }

        var coverage = new ReportShapes.Coverage(
            sessions.Count,
            KRule.People(sessionPeople, k),
            sessions.Count(s => !s.Row.PersonMapped),
            sessions
                .GroupBy(s => s.Row.Agent)
                .OrderByDescending(g => g.Count())
                .ThenBy(g => g.Key, StringComparer.Ordinal)
                .Select(g => new ReportShapes.AgentCoverage(
                    g.Key,
                    g.Count(),
                    g.Select(s => s.Row.Source).Distinct().Order().ToList()
                ))
                .ToList(),
            org.Settings.PromptMode is { } mode ? SteeringFacts.Enum(mode) : null,
            facts.Count,
            facts.Count(f => f.Row.Status == "pending"),
            facts.Count(f => f.Row.Status == "unclassified" && f.Row.Signal != "abandoned"),
            await connection.ExecuteScalarAsync<bool>(
                new CommandDefinition(
                    "SELECT EXISTS (SELECT 1 FROM casebox.workers WHERE org_id = @Org AND last_seen_at > now() - interval '10 minutes')",
                    args,
                    cancellationToken: ct
                )
            )
        );

        var patterns = (
            await connection.QueryAsync<(
                string Id,
                string Title,
                string Status,
                bool Advisory,
                string WentWrong,
                string? Label,
                string Workspace
            )>(
                new CommandDefinition(
                    "SELECT id, title, status, advisory, went_wrong, label, workspace FROM casebox.patterns WHERE org_id = @Org AND status <> 'dismissed' AND (@Workspace::text IS NULL OR workspace = @Workspace)",
                    new { Org, q.Workspace },
                    cancellationToken: ct
                )
            )
        ).ToList();
        // A pattern is a correction cluster: it shows only with k mapped people behind it now.
        var shown =
            new List<(
                string Id,
                string Title,
                string Status,
                bool Advisory,
                string WentWrong,
                string? Label,
                string Workspace
            )>();
        foreach (var p in patterns)
            if (
                (
                    await Patterns.PatternEndpoints.EvidenceAsync(connection, Org, p.Id, k, ct)
                )?.MeetsK == true
            )
                shown.Add(p);
        patterns = shown;
        themes = themes
            .Select(t =>
                t with
                {
                    Patterns = patterns
                        .Where(p =>
                            p.WentWrong == t.WentWrong
                            && (
                                t.WentWrong != "other"
                                || string.Equals(
                                    p.Label?.Trim(),
                                    t.Label,
                                    StringComparison.OrdinalIgnoreCase
                                )
                            )
                        )
                        .Select(p => new ReportShapes.ThemePattern(
                            p.Id,
                            p.Title,
                            p.Status,
                            p.Advisory
                        ))
                        .ToList(),
                }
            )
            .ToList();

        return new ReportShapes.Report(
            new ReportShapes.Period(q.From, q.To),
            k.K,
            k.Solo,
            coverage,
            headline,
            themes,
            mix,
            afterMerge,
            groups,
            new ReportShapes.Hidden(hiddenThemes, hiddenGroups, hiddenMix, hiddenHeadline)
        );
    }

    // Finished work items of the period: resolved, or with a merged pull request. Only agent work
    // counts: an item needs a captured session or an agent pull request.
    private async Task<List<Item>> ItemsAsync(
        NpgsqlConnection connection,
        OrgSettings settings,
        Context dims,
        object args,
        CancellationToken ct
    )
    {
        var finished = (
            await connection.QueryAsync<(string Id, string? Type, string? Labels)>(
                new CommandDefinition(
                    """
                    WITH finished AS (
                        SELECT t.work_item_id AS id FROM casebox.work_item_timeline t
                        WHERE t.org_id = @Org AND (t.kind = 'merged' OR (t.kind = 'snapshot' AND (t.detail->>'resolved')::boolean))
                        GROUP BY t.work_item_id HAVING min(t.at) >= @From AND min(t.at) < @To)
                    SELECT w.id, w.type, (w.snapshot->'labels')::text FROM finished JOIN casebox.work_items w ON w.org_id = @Org AND w.id = finished.id
                    """,
                    args,
                    cancellationToken: ct
                )
            )
        ).ToList();
        var ids = finished.Select(f => f.Id).ToArray();
        var withIds = new DynamicParameters(args);
        withIds.Add("Ids", ids);

        var sessions = (
            await connection.QueryAsync<ItemSession>(
                new CommandDefinition(
                    """
                    SELECT work_item_id, agent, model, repo, harness_version, task_type, person, person_mapped, period
                    FROM casebox.sessions WHERE org_id = @Org AND work_item_id = ANY(@Ids) AND repo IS NOT NULL
                    """,
                    withIds,
                    cancellationToken: ct
                )
            )
        ).ToLookup(s => s.WorkItemId);
        var prs = (
            await connection.QueryAsync<(
                string WorkItemId,
                string Repo,
                string? Author,
                bool Mapped,
                bool Bot,
                DateTime CreatedAt
            )>(
                new CommandDefinition(
                    """
                    SELECT work_item_id, repo, snapshot->'author'->>'token', coalesce((snapshot->'author'->>'mapped')::boolean, false),
                           coalesce((snapshot->'author'->>'bot')::boolean, false), (snapshot->>'createdAt')::timestamptz
                    FROM casebox.pull_requests WHERE org_id = @Org AND work_item_id = ANY(@Ids) AND is_agent
                    """,
                    withIds,
                    cancellationToken: ct
                )
            )
        ).ToLookup(p => p.WorkItemId);
        var corrections = (
            await connection.QueryAsync<ItemCorrection>(
                new CommandDefinition(
                    """
                    SELECT coalesce(s.work_item_id, p.work_item_id) AS work_item_id, f.phase, f.person, f.person_mapped, f.period
                    FROM casebox.steering_facts f
                    LEFT JOIN casebox.sessions s ON s.org_id = f.org_id AND s.id = f.session_id
                    LEFT JOIN casebox.pull_requests p ON f.session_id IS NULL AND p.org_id = f.org_id AND p.repo = f.repo AND p.number = f.number
                    WHERE f.org_id = @Org AND f.intent = 'correction' AND coalesce(s.work_item_id, p.work_item_id) = ANY(@Ids)
                    """,
                    withIds,
                    cancellationToken: ct
                )
            )
        ).ToLookup(c => c.WorkItemId);

        var items = new List<Item>();
        foreach (var (id, type, labels) in finished)
        {
            var itemTask = TaskTypes.Of(type, labels);
            var itemSessions = sessions[id].ToList();
            var itemPrs = prs[id].ToList();
            if (itemSessions.Count == 0 && itemPrs.Count == 0)
                continue;
            var itemDims = Dims.Union(
                itemSessions
                    .Select(s =>
                        dims.Of(s.Repo, s.Agent, s.Model, s.HarnessVersion, itemTask ?? s.TaskType)
                    )
                    .Concat(itemPrs.Select(p => dims.Of(p.Repo, null, null, null, itemTask)))
            );
            var itemCorrections = corrections[id].ToList();
            var people = itemSessions
                .Select(s => new Person(s.Person, s.PersonMapped, s.Period))
                .Concat(
                    itemPrs
                        .Where(p => p.Author is not null && !p.Bot)
                        .Select(p => new Person(
                            p.Author!,
                            p.Mapped,
                            Identities.PeriodOf(settings.PseudonymPeriod, p.CreatedAt)
                        ))
                )
                .Concat(itemCorrections.Select(c => new Person(c.Person, c.PersonMapped, c.Period)))
                .ToList();
            items.Add(
                new Item(
                    itemDims,
                    people,
                    itemCorrections.Count(c => c.Phase == "in_session"),
                    itemCorrections.Count(c => c.Phase == "before_merge"),
                    itemCorrections.Count(c => c.Phase == "after_merge")
                )
            );
        }

        return items;
    }

    private static Person PersonOf(FactRow f) => new(f.Person, f.PersonMapped, f.Period);

    private static ReportShapes.Rate? CorrectionFree(IReadOnlyCollection<Item> items) =>
        items.Count == 0 ? null : RateOf(items.Count(i => i.Clean), items.Count);

    private static ReportShapes.PerItem? PerItem(IReadOnlyCollection<Item> items) =>
        items.Count == 0
            ? null
            : new ReportShapes.PerItem(
                SpreadOf(items.Select(i => (double)i.InSession)),
                SpreadOf(items.Select(i => (double)i.BeforeMerge)),
                SpreadOf(items.Select(i => (double)i.AfterMerge)),
                items.Count
            );

    // Agent turns and tool calls before the first in-session correction. Sessions whose
    // interventions are not all classified are left out: their first correction is unknown.
    private static ReportShapes.AutonomousRun? Autonomous(IEnumerable<SessionRow> all)
    {
        var sessions = all.Where(s => s.Active && !s.Uncertain).ToList();
        if (sessions.Count == 0)
            return null;
        var corrected = sessions.Where(s => s.FirstCorrection is not null).ToList();
        return new ReportShapes.AutonomousRun(
            SpreadOf(corrected.Select(s => (double)s.Turns)),
            SpreadOf(corrected.Select(s => (double)s.Tools)),
            corrected.Count,
            Round((double)(sessions.Count - corrected.Count) / sessions.Count)
        );
    }

    private static ReportShapes.Rate? AfterMergeRate(IEnumerable<PrRow> all)
    {
        var prs = all.ToList();
        return prs.Count == 0 ? null : RateOf(prs.Count(p => p.Corrected), prs.Count);
    }

    // Ended sessions with agent activity and a known commit count.
    private static bool Counted(SessionRow s) =>
        s.EndedAt is not null && s.Active && s.Commits is not null;

    private static ReportShapes.Rate? Abandonment(IEnumerable<SessionRow> all)
    {
        var sessions = all.Where(Counted).ToList();
        return sessions.Count == 0
            ? null
            : RateOf(sessions.Count(s => s.Abandoned), sessions.Count);
    }

    private static (IReadOnlyList<ReportShapes.Theme>, int Hidden) Themes(
        IReadOnlyList<FactRow> corrections,
        KView k
    )
    {
        var themes = new List<ReportShapes.Theme>();
        var hidden = 0;
        foreach (
            var g in corrections.GroupBy(f =>
                (
                    f.WentWrong!,
                    Label: f.WentWrong == "other"
                        ? f.WentWrongLabel?.Trim().ToLowerInvariant()
                        : null
                )
            )
        )
        {
            var people = g.Select(PersonOf).ToList();
            if (!KRule.Meets(people, k))
            {
                hidden++;
                continue;
            }

            var list = g.ToList();
            var withPrevention = list.Where(f => f.Prevention is not null).ToList();
            var fixable =
                withPrevention.Count > 0
                && withPrevention.Count(f => HarnessFixable.Contains(f.Prevention!)) * 2
                    > withPrevention.Count;
            var prevention = withPrevention
                .GroupBy(f => f.Prevention!)
                .OrderByDescending(p => p.Count())
                .ThenBy(p => p.Key, StringComparer.Ordinal)
                .Select(p => new ReportShapes.PreventionShare(
                    p.Key,
                    Round((double)p.Count() / withPrevention.Count)
                ))
                .ToList();
            themes.Add(
                new ReportShapes.Theme(
                    g.Key.Item1,
                    g.Key.Label,
                    list.Count,
                    KRule.People(people, k),
                    fixable,
                    prevention,
                    new ReportShapes.Phases(
                        list.Count(f => f.Phase == "in_session"),
                        list.Count(f => f.Phase == "before_merge"),
                        list.Count(f => f.Phase == "after_merge")
                    ),
                    Quotes(list)
                )
            );
        }

        return (
            themes
                .OrderByDescending(t => t.Corrections)
                .ThenBy(t => t.WentWrong, StringComparer.Ordinal)
                .ThenBy(t => t.Label, StringComparer.Ordinal)
                .ToList(),
            hidden
        );
    }

    // Up to three quotes, from three different people when there are three: masked, without
    // authors, with the day only. A person's relabel marks a good example, so it comes first.
    private static IReadOnlyList<ReportShapes.Quote> Quotes(IReadOnlyList<FactRow> facts)
    {
        var candidates = facts
            .Where(f => !string.IsNullOrWhiteSpace(f.Text))
            .OrderByDescending(f => f.LabelSource == "human")
            .ThenByDescending(f => f.Confidence ?? 0)
            .ThenByDescending(f => f.At)
            .ThenBy(f => f.Ref, StringComparer.Ordinal)
            .ToList();
        var picked = candidates.DistinctBy(f => f.Person).Take(3).ToList();
        picked.AddRange(candidates.Where(c => !picked.Contains(c)).Take(3 - picked.Count));
        return picked
            .Select(f => new ReportShapes.Quote(f.Ref, Clip(Masking.Mask(f.Text)!, 600), Day(f.At)))
            .ToList();
    }

    private static (IReadOnlyList<ReportShapes.MixEntry>, int Hidden) PreventionMix(
        IReadOnlyList<FactRow> corrections,
        KView k
    )
    {
        var mix = new List<ReportShapes.MixEntry>();
        var hidden = 0;
        foreach (
            var g in corrections
                .GroupBy(f => f.Prevention!)
                .OrderByDescending(g => g.Count())
                .ThenBy(g => g.Key, StringComparer.Ordinal)
        )
        {
            if (!KRule.Meets(g.Select(PersonOf), k))
            {
                hidden++;
                continue;
            }

            mix.Add(
                new ReportShapes.MixEntry(
                    g.Key,
                    Round((double)g.Count() / corrections.Count),
                    g.Count(),
                    HarnessFixable.Contains(g.Key)
                )
            );
        }

        return (mix, hidden);
    }

    public static string Day(DateTime at) =>
        DateTime
            .SpecifyKind(at, DateTimeKind.Utc)
            .ToString("yyyy-MM-dd", System.Globalization.CultureInfo.InvariantCulture);

    public static string Clip(string text, int max) =>
        text.Length <= max ? text : text[..max] + "…";

    public static ReportShapes.Rate RateOf(int successes, int n) =>
        new(Round((double)successes / n), n, Wilson(successes, n));

    // The 95% Wilson score interval of a proportion.
    public static double[] Wilson(int successes, int n)
    {
        const double z = 1.959964;
        var p = (double)successes / n;
        var denominator = 1 + z * z / n;
        var centre = (p + z * z / (2 * n)) / denominator;
        var half = z * Math.Sqrt(p * (1 - p) / n + z * z / (4.0 * n * n)) / denominator;
        return [Round(Math.Max(0, centre - half)), Round(Math.Min(1, centre + half))];
    }

    // The median and the 75th percentile, interpolated between ranks.
    public static ReportShapes.Spread? SpreadOf(IEnumerable<double> values)
    {
        var sorted = values.Order().ToArray();
        if (sorted.Length == 0)
            return null;
        return new ReportShapes.Spread(Round(Quantile(sorted, 0.5)), Round(Quantile(sorted, 0.75)));
    }

    private static double Quantile(double[] sorted, double q)
    {
        var position = (sorted.Length - 1) * q;
        var lower = (int)Math.Floor(position);
        var upper = (int)Math.Ceiling(position);
        return sorted[lower] + (sorted[upper] - sorted[lower]) * (position - lower);
    }

    private static double Round(double value) => Math.Round(value, 4);
}

// Task type from the work item first (docs/specs/steering.md, Harness version and task type).
public static class TaskTypes
{
    public static string? Of(string? type, string? labelsJson)
    {
        var labels = labelsJson is null
            ? []
            : JsonSerializer.Deserialize<string[]>(labelsJson) ?? [];
        foreach (var label in labels.Select(l => l.Trim().ToLowerInvariant()))
        {
            switch (label)
            {
                case "refactor" or "refactoring":
                    return "refactor";
                case "test" or "tests" or "testing":
                    return "test";
                case "config" or "configuration":
                    return "config";
                case "docs" or "documentation":
                    return "docs";
            }
        }

        switch (type?.Trim().ToLowerInvariant())
        {
            case "bug":
                return "bug";
            case "story" or "feature" or "new feature" or "improvement" or "epic":
                return "feature";
        }

        if (labels.Any(l => l.Equals("bug", StringComparison.OrdinalIgnoreCase)))
            return "bug";
        if (
            labels.Any(l =>
                l.Equals("enhancement", StringComparison.OrdinalIgnoreCase)
                || l.Equals("feature", StringComparison.OrdinalIgnoreCase)
            )
        )
            return "feature";
        return null;
    }
}
