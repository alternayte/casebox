using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Steering;

public static class SteeringEndpoints
{
    public const int PageSize = 50;

    public sealed record RelabelRequest(string Ref, string Intent, string? WentWrong, string? WentWrongLabel, string? Prevention);

    public sealed record RefreshRequest(string? Repo);

    public sealed record Status(int Sessions, int Interventions, int Pending, int Unclassified, bool WorkerSeen, int AnalysisWorkers);

    public sealed record InterventionView(string Ref, string Signal, string Phase, string Day, string? Text, string? Intent, string? WentWrong, string? WentWrongLabel,
        string? Prevention, string? LabelSource, double? Confidence);

    public sealed record InterventionPage(IReadOnlyList<InterventionView> Interventions, int Page, int PageSize, int Total);

    public sealed record StepAgreement(double Agreement, double Kappa, int N);

    public sealed record AgreementReport(int People, int Events, StepAgreement? Intent, StepAgreement? WentWrong, StepAgreement? Prevention);

    public static void MapSteering(this RouteGroupBuilder api)
    {
        var steering = api.MapGroup("/steering").WithTags("Steering").RequireAuthorization(Policies.Viewer);

        steering.MapGet("/report", async (HttpContext http, SteeringReports reports, TimeProvider clock) =>
            Results.Ok(await reports.BuildAsync(QueryOf(http.Request.Query, clock), http.RequestAborted)));

        // A theme's interventions, for relabeling. They are shown only when the theme itself meets k.
        steering.MapGet("/interventions", async (string wentWrong, int? page, HttpContext http, NpgsqlDataSource db, IEventStore store, TimeProvider clock) =>
        {
            var q = QueryOf(http.Request.Query, clock);
            var (org, _) = await store.Load<Organisation>(Organisation.StreamId, http.RequestAborted);
            var unclassified = wentWrong == "unclassified";
            if (!unclassified && SteeringFacts.Parse<WentWrong>(ValidEnum<WentWrong>(wentWrong)) is null)
                throw new DomainException($"'{wentWrong}' is not a theme.");

            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var workspaceRepos = q.Workspace is null ? null : (await connection.QueryAsync<string>(new CommandDefinition(
                "SELECT jsonb_array_elements_text(repos) FROM casebox.workspaces WHERE org_id = @Org AND name = @Name",
                new { Org = http.User.OrgId(), Name = q.Workspace }, cancellationToken: http.RequestAborted))).ToHashSet(StringComparer.Ordinal);
            var rows = (await connection.QueryAsync<ListedFact>(new CommandDefinition(
                """
                SELECT f.ref, f.signal, f.phase, f.at, f.repo, f.text, f.intent, f.went_wrong, f.went_wrong_label, f.prevention, f.label_source, f.confidence,
                       f.person, f.person_mapped, f.period,
                       coalesce(s.agent, ps.agent) AS agent, coalesce(s.model, ps.model) AS model, coalesce(s.harness_version, ps.harness_version) AS harness,
                       coalesce(s.task_type, ps.task_type) AS session_task, w.type AS item_type, (w.snapshot->'labels')::text AS item_labels
                FROM casebox.steering_facts f
                LEFT JOIN casebox.sessions s ON s.org_id = f.org_id AND s.id = f.session_id
                LEFT JOIN casebox.pull_requests p ON f.session_id IS NULL AND p.org_id = f.org_id AND p.repo = f.repo AND p.number = f.number
                LEFT JOIN LATERAL (SELECT x.agent, x.model, x.harness_version, x.task_type FROM casebox.sessions x
                                   WHERE x.org_id = p.org_id AND x.repo = p.repo AND x.branch = p.head_ref ORDER BY x.started_at LIMIT 1) ps ON true
                LEFT JOIN casebox.work_items w ON w.org_id = f.org_id AND w.id = coalesce(s.work_item_id, p.work_item_id)
                WHERE f.org_id = @Org AND f.at >= @From AND f.at < @To
                  AND (CASE WHEN @Unclassified THEN f.status = 'unclassified' AND f.unclassified_reason <> 'no_text'
                            ELSE f.intent = 'correction' AND f.went_wrong = @WentWrong END)
                ORDER BY f.at DESC, f.ref
                """,
                new { Org = http.User.OrgId(), q.From, q.To, Unclassified = unclassified, WentWrong = wentWrong }, cancellationToken: http.RequestAborted)))
                .Where(r => Passes(r, q, workspaceRepos)).ToList();

            if (!KRule.Meets(rows.Select(r => new Person(r.Person, r.PersonMapped, r.Period)), org.Settings.K))
                return Results.Problem(statusCode: StatusCodes.Status403Forbidden, title: $"Fewer than {org.Settings.K} people are behind this theme, so its interventions stay hidden.");

            var number = Math.Max(1, page ?? 1);
            var shown = rows.Skip((number - 1) * PageSize).Take(PageSize)
                .Select(r => new InterventionView(r.Ref, r.Signal, r.Phase, SteeringReports.Day(r.At), Masking.Mask(r.Text), r.Intent, r.WentWrong, r.WentWrongLabel, r.Prevention, r.LabelSource, r.Confidence))
                .ToList();
            return Results.Ok(new InterventionPage(shown, number, PageSize, rows.Count));
        });

        // A person corrects the labels. Relabels become the classifier's examples.
        steering.MapPost("/relabel", async (RelabelRequest body, HttpContext http, NpgsqlDataSource db, IEventStore store) =>
        {
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var target = await connection.QuerySingleOrDefaultAsync<(string Stream, string Id)?>(new CommandDefinition(
                "SELECT stream_id, intervention_id FROM casebox.steering_facts WHERE org_id = @Org AND ref = @Ref",
                new { Org = http.User.OrgId(), Ref = body.Ref ?? "" }, cancellationToken: http.RequestAborted));
            if (target is null) return Results.NotFound();
            var labels = new Labels(
                SteeringFacts.Parse<Intent>(ValidEnum<Intent>(body.Intent)) ?? throw new DomainException("Name the intent."),
                SteeringFacts.Parse<WentWrong>(ValidEnum<WentWrong>(body.WentWrong)),
                string.IsNullOrWhiteSpace(body.WentWrongLabel) ? null : body.WentWrongLabel.Trim(),
                SteeringFacts.Parse<Prevention>(ValidEnum<Prevention>(body.Prevention)));
            var by = http.User.AccountId() ?? throw new DomainException("Only an account can relabel.");
            await store.Execute<SteeringState>(target.Value.Stream, s => SteeringDecider.Relabel(s, target.Value.Id, labels, by), http.RequestAborted);
            return Results.NoContent();
        }).RequireAuthorization(Policies.Member);

        steering.MapGet("/status", async (string? repo, HttpContext http, NpgsqlDataSource db) =>
            Results.Ok(await StatusAsync(db, http.User.OrgId(), repo, http.RequestAborted)));

        // Detection now, for `casebox import` and the Detect button: the minute loop would find the
        // same interventions a little later.
        steering.MapPost("/refresh", async (RefreshRequest? body, HttpContext http, SteeringScan scan, NpgsqlDataSource db) =>
        {
            var repo = string.IsNullOrWhiteSpace(body?.Repo) ? null : Workspaces.WorkspaceDecider.NormalizeRepo(body.Repo);
            await scan.RunAsync(repo, http.RequestAborted);
            return Results.Ok(await StatusAsync(db, http.User.OrgId(), repo, http.RequestAborted));
        }).RequireAuthorization(Policies.Member);

        // How often the model's labels agree with the team's relabels (SDD section 6).
        steering.MapGet("/agreement", async (HttpContext http, NpgsqlDataSource db, IEventStore store) =>
        {
            var (org, _) = await store.Load<Organisation>(Organisation.StreamId, http.RequestAborted);
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            var rows = (await connection.QueryAsync<(string Person, bool PersonMapped, string Period, string Intent, string? WentWrong, string? Prevention, string ModelIntent, string? ModelWentWrong, string? ModelPrevention)>(
                new CommandDefinition(
                    """
                    SELECT person, person_mapped, period, intent, went_wrong, prevention, model_intent, model_went_wrong, model_prevention
                    FROM casebox.steering_facts WHERE org_id = @Org AND label_source = 'human' AND model_intent IS NOT NULL
                    """,
                    new { Org = http.User.OrgId() }, cancellationToken: http.RequestAborted))).ToList();
            var people = rows.Select(r => new Person(r.Person, r.PersonMapped, r.Period)).ToList();
            if (!KRule.Meets(people, org.Settings.K)) return Results.Ok(new { hidden = true, k = org.Settings.K });

            var corrections = rows.Where(r => r.Intent == "correction" && r.ModelIntent == "correction").ToList();
            return Results.Ok(new AgreementReport(KRule.People(people), rows.Count,
                Agreement(rows.Select(r => (r.Intent, r.ModelIntent)).ToList()),
                Agreement(corrections.Where(r => r.WentWrong is not null && r.ModelWentWrong is not null).Select(r => (r.WentWrong!, r.ModelWentWrong!)).ToList()),
                Agreement(corrections.Where(r => r.Prevention is not null && r.ModelPrevention is not null).Select(r => (r.Prevention!, r.ModelPrevention!)).ToList())));
        });
    }

    private sealed record ListedFact(string Ref, string Signal, string Phase, DateTime At, string Repo, string? Text, string? Intent, string? WentWrong, string? WentWrongLabel,
        string? Prevention, string? LabelSource, double? Confidence, string Person, bool PersonMapped, string Period, string? Agent, string? Model, string? Harness,
        string? SessionTask, string? ItemType, string? ItemLabels);

    private static bool Passes(ListedFact f, ReportQuery q, IReadOnlySet<string>? workspaceRepos) =>
        (q.Repo is null || f.Repo == q.Repo) && (q.Agent is null || (f.Agent ?? "unknown") == q.Agent) && (q.Model is null || (f.Model ?? "unknown") == q.Model)
        && (q.Harness is null || (f.Harness ?? "unknown") == q.Harness)
        && (q.TaskType is null || (TaskTypes.Of(f.ItemType, f.ItemLabels) ?? f.SessionTask ?? "unknown") == q.TaskType)
        && (workspaceRepos is null || workspaceRepos.Contains(f.Repo));

    // Cohen's kappa: agreement beyond what two raters with these label frequencies reach by chance.
    public static StepAgreement? Agreement(IReadOnlyList<(string Human, string Model)> pairs)
    {
        if (pairs.Count == 0) return null;
        var n = (double)pairs.Count;
        var observed = pairs.Count(p => p.Human == p.Model) / n;
        var labels = pairs.SelectMany(p => new[] { p.Human, p.Model }).Distinct();
        var chance = labels.Sum(l => pairs.Count(p => p.Human == l) / n * (pairs.Count(p => p.Model == l) / n));
        var kappa = chance >= 1 ? 1 : (observed - chance) / (1 - chance);
        return new StepAgreement(Math.Round(observed, 4), Math.Round(kappa, 4), pairs.Count);
    }

    public static async Task<Status> StatusAsync(NpgsqlDataSource db, string org, string? repo, CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        return await connection.QuerySingleAsync<Status>(new CommandDefinition(
            """
            SELECT (SELECT count(*) FROM casebox.sessions WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo))::int AS sessions,
                   (SELECT count(*) FROM casebox.steering_facts WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo))::int AS interventions,
                   (SELECT count(*) FROM casebox.steering_facts WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo) AND status = 'pending')::int AS pending,
                   (SELECT count(*) FROM casebox.steering_facts WHERE org_id = @Org AND (@Repo::text IS NULL OR repo = @Repo) AND status = 'unclassified' AND signal <> 'abandoned')::int AS unclassified,
                   EXISTS (SELECT 1 FROM casebox.workers WHERE org_id = @Org AND last_seen_at > now() - interval '10 minutes') AS worker_seen,
                   (SELECT count(*) FROM casebox.workers WHERE org_id = @Org AND last_seen_at > now() - interval '10 minutes' AND @Kind = ANY(kinds))::int AS analysis_workers
            """,
            new { Org = org, Repo = repo, Kind = SteeringJobs.Classify }, cancellationToken: ct));
    }

    // The report's period and filters. The period defaults to the last 30 days.
    public static ReportQuery QueryOf(IQueryCollection query, TimeProvider clock)
    {
        DateTimeOffset? Time(string name) =>
            query[name].FirstOrDefault() is { Length: > 0 } s
                ? DateTimeOffset.TryParse(s, System.Globalization.CultureInfo.InvariantCulture, System.Globalization.DateTimeStyles.AssumeUniversal, out var t) ? t.ToUniversalTime() : throw new DomainException($"'{name}' is not a time.")
                : null;
        string? Text(string name) => query[name].FirstOrDefault() is { Length: > 0 } s ? s : null;

        var to = Time("to") ?? clock.GetUtcNow();
        var from = Time("from") ?? to.AddDays(-30);
        if (from >= to) throw new DomainException("'from' must come before 'to'.");
        if (to - from > TimeSpan.FromDays(400)) throw new DomainException("A report covers at most 400 days.");
        Dimension? groupBy = Text("groupBy") is { } g
            ? Enum.TryParse<Dimension>(g.Replace("_", ""), ignoreCase: true, out var d) ? d : throw new DomainException($"'{g}' is not a dimension to group by.")
            : null;
        var repo = Text("repo") is { } r ? Workspaces.WorkspaceDecider.NormalizeRepo(r) : null;
        return new ReportQuery(from, to, Text("workspace"), repo, Text("agent"), Text("model"), Text("harness"), Text("taskType"), groupBy);
    }

    private static string? ValidEnum<T>(string? value) where T : struct, Enum
    {
        if (string.IsNullOrWhiteSpace(value)) return null;
        try
        {
            SteeringFacts.Parse<T>(value);
            return value;
        }
        catch (System.Text.Json.JsonException)
        {
            throw new DomainException($"'{value}' is not a valid {typeof(T).Name}.");
        }
    }
}
