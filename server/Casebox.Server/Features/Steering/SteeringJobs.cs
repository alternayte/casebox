using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Steering;

public static class SteeringJobs
{
    public const string Classify = "steering.classify";
    public const string PullRequest = "steering.pr";
    public const int PerJob = 20;

    // Below this confidence an intervention is unclassified, never guessed (SDD section 6).
    public const double Threshold = 0.6;
}

// The worker's answer to a steering.classify job (docs/specs/steering.md, Jobs).
public sealed record ClassifyResult(
    string? Model,
    string? PromptVersion,
    IReadOnlyList<ClassifyItem>? Results,
    string? TaskType
);

public sealed record ClassifyItem(
    string InterventionId,
    string? Intent,
    string? WentWrong,
    string? WentWrongLabel,
    string? Prevention,
    double Confidence,
    string? Error
);

public sealed class ClassifyResultHandler : IJobResultHandler
{
    public string Kind => SteeringJobs.Classify;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        result.Services.GetService<Patterns.PatternScheduler>()?.Nudge();
        var payload = result.Job.Payload;
        var stream = payload.GetProperty("stream").GetString()!;
        var answer =
            result.Result.Deserialize<ClassifyResult>(JsonSerializerOptions.Web)
            ?? throw new DomainException("The result is empty.");
        var model = answer.Model ?? "unknown";
        var version = answer.PromptVersion ?? "unknown";
        var byId = (answer.Results ?? [])
            .GroupBy(r => r.InterventionId)
            .ToDictionary(g => g.Key, g => g.Last(), StringComparer.Ordinal);

        var ids = payload
            .GetProperty("interventionIds")
            .EnumerateArray()
            .Select(e => e.GetString()!)
            .ToList();
        if (ids.Count > 0)
        {
            var (state, _) = await result.Store.Load<SteeringState>(stream, ct);
            var events = new List<object>();
            foreach (var id in ids.Where(state.Interventions.ContainsKey))
            {
                var item = byId.GetValueOrDefault(id);
                var intervention = state.Interventions[id];
                object decided = Decide(intervention, id, item, model, version);
                var next = decided is SteeringEvents.Classified c
                    ? SteeringDecider.Classify(state, c)
                    : SteeringDecider.Unclassify(state, (SteeringEvents.Unclassified)decided);
                foreach (var e in next)
                {
                    events.Add(e);
                    state = SteeringState.Evolve(state, e);
                }
            }

            if (events.Count > 0)
                await result.Store.Execute<SteeringState>(stream, _ => events, ct);
        }

        if (
            payload.TryGetProperty("taskFor", out var taskFor)
            && taskFor.GetString() is { } session
            && SteeringFacts.Parse<TaskType>(answer.TaskType) is { } taskType
        )
            await result.Transaction.Connection!.ExecuteAsync(
                new CommandDefinition(
                    "UPDATE casebox.sessions SET task_type = @Type WHERE org_id = @Org AND id = @Id",
                    new
                    {
                        Org = result.OrgId,
                        Id = session,
                        Type = SteeringFacts.Enum(taskType),
                    },
                    result.Transaction,
                    cancellationToken: ct
                )
            );
    }

    // Valid labels at or above the threshold are a classification; anything else is unclassified.
    private static object Decide(
        Intervention intervention,
        string id,
        ClassifyItem? item,
        string model,
        string version
    )
    {
        if (item is null || item.Error is not null)
            return new SteeringEvents.Unclassified(
                id,
                UnclassifiedReason.ModelError,
                null,
                model,
                version
            );
        var intent = intervention.RuleIntent ?? TryParse<Intent>(item.Intent);
        var labels = intent is { } i
            ? new Labels(
                i,
                TryParse<WentWrong>(item.WentWrong),
                string.IsNullOrWhiteSpace(item.WentWrongLabel) ? null : item.WentWrongLabel.Trim(),
                TryParse<Prevention>(item.Prevention)
            )
            : null;
        if (
            labels is null
            || labels.Problem() is not null
            || item.Confidence is < 0 or > 1
            || (item.WentWrong is not null && labels.WentWrong is null)
            || (item.Prevention is not null && labels.Prevention is null)
        )
            return new SteeringEvents.Unclassified(
                id,
                UnclassifiedReason.InvalidOutput,
                item.Confidence,
                model,
                version
            );
        if (item.Confidence < SteeringJobs.Threshold)
            return new SteeringEvents.Unclassified(
                id,
                UnclassifiedReason.LowConfidence,
                item.Confidence,
                model,
                version
            );
        return new SteeringEvents.Classified(
            id,
            labels.Intent,
            labels.WentWrong,
            labels.WentWrongLabel,
            labels.Prevention,
            item.Confidence,
            model,
            version
        );
    }

    private static T? TryParse<T>(string? value)
        where T : struct, Enum
    {
        try
        {
            return SteeringFacts.Parse<T>(value);
        }
        catch (JsonException)
        {
            return null;
        }
    }
}

// The worker's answer to a steering.pr job: human rewrites of agent lines, and review comments
// whose lines changed afterwards.
public sealed record PullRequestResult(
    IReadOnlyList<Rewrite>? Rewrites,
    IReadOnlyList<ReviewChange>? ReviewChanges
);

public sealed record Rewrite(string Sha, int Lines, IReadOnlyList<string>? Files);

public sealed record ReviewChange(long CommentId, string Sha);

public sealed class PullRequestResultHandler : IJobResultHandler
{
    public string Kind => SteeringJobs.PullRequest;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var repo = result.Job.Payload.GetProperty("repo").GetString()!;
        var number = result.Job.Payload.GetProperty("number").GetInt32();
        var answer =
            result.Result.Deserialize<PullRequestResult>(JsonSerializerOptions.Web)
            ?? new PullRequestResult([], []);
        var snapshot = await result.Transaction.Connection!.QuerySingleOrDefaultAsync<string>(
            new CommandDefinition(
                "SELECT snapshot::text FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number",
                new
                {
                    Org = result.OrgId,
                    Repo = repo,
                    Number = number,
                },
                result.Transaction,
                cancellationToken: ct
            )
        );
        if (snapshot is null)
            return;
        var pr = JsonSerializer.Deserialize<PullRequestSnapshot>(snapshot, GitHubJson.Options)!;
        var (org, _) = await result.Store.Load<Organisation>(Organisation.StreamId, ct);
        var period = Capture.Identities.PeriodOf(org.Settings.PseudonymPeriod, pr.CreatedAt);

        var observed = new List<SteeringEvents.Observed>();
        foreach (var rewrite in answer.Rewrites ?? [])
        {
            if (
                rewrite.Lines <= 0
                || pr.Commits.FirstOrDefault(c => c.Sha == rewrite.Sha)
                    is not { Author: { Bot: false } author } commit
            )
                continue;
            observed.Add(
                new SteeringEvents.Observed(
                    $"rewrite:{commit.Sha}",
                    Signal.HumanRewrite,
                    Phase.BeforeMerge,
                    commit.At,
                    repo,
                    null,
                    number,
                    author.Token,
                    author.Mapped,
                    period,
                    commit.Message,
                    Intent.Correction,
                    new SteeringRefs(Commits: [commit.Sha])
                )
            );
        }

        foreach (var change in answer.ReviewChanges ?? [])
        {
            if (
                pr.ReviewComments.FirstOrDefault(c => c.Id == change.CommentId)
                is not { Author: { Bot: false } author } comment
            )
                continue;
            if (pr.Commits.All(c => c.Sha != change.Sha))
                continue;
            observed.Add(
                new SteeringEvents.Observed(
                    $"review:{comment.Id}",
                    Signal.ReviewChange,
                    Phase.BeforeMerge,
                    comment.At,
                    repo,
                    null,
                    number,
                    author.Token,
                    author.Mapped,
                    period,
                    comment.Body,
                    null,
                    new SteeringRefs(Commits: [change.Sha], CommentId: comment.Id)
                )
            );
        }

        if (observed.Count > 0)
            await result.Store.Execute<SteeringState>(
                SteeringState.PullRequestStream(repo, number),
                s => SteeringDecider.Observe(s, observed),
                ct
            );
    }
}

// What a worker reads to classify: the windows of docs/specs/steering.md, with every person token
// masked, and the team's relabels as examples. The classifier never sees who anyone is.
public static class SteeringWorkerEndpoints
{
    public sealed record WindowsRequest(
        string Stream,
        IReadOnlyList<string>? InterventionIds,
        string? TaskFor
    );

    public sealed record Turn(string? Text, IReadOnlyList<ToolUse> Tools);

    public sealed record ToolUse(string? Name, string? Status, IReadOnlyList<string>? Files);

    public sealed record Window(
        string InterventionId,
        string Signal,
        string Phase,
        string? RuleIntent,
        string? Agent,
        string? Model,
        string? Human,
        Turn Before,
        Turn After,
        IReadOnlyList<string> Files,
        string Repo,
        IReadOnlyList<string> Commits
    );

    public sealed record TaskText(string SessionId, string? Text);

    public sealed record Example(
        Window Window,
        string Intent,
        string? WentWrong,
        string? WentWrongLabel,
        string? Prevention
    );

    public static void MapSteeringWorker(this RouteGroupBuilder worker)
    {
        var steering = worker
            .MapGroup("/steering")
            .WithTags("Worker")
            .RequireAuthorization(Policies.Worker);

        steering.MapPost(
            "/windows",
            async (WindowsRequest body, HttpContext http, SteeringWindows windows) =>
            {
                var ids = body.InterventionIds ?? [];
                if (ids.Count > SteeringJobs.PerJob)
                    throw new DomainException(
                        $"Ask for at most {SteeringJobs.PerJob} windows at once."
                    );
                var built = await windows.ForAsync(body.Stream ?? "", ids, http.RequestAborted);
                var task = body.TaskFor is { } session
                    ? await windows.TaskAsync(session, http.RequestAborted)
                    : null;
                return Results.Ok(new { windows = built, task });
            }
        );

        steering.MapGet(
            "/examples",
            async (HttpContext http, SteeringWindows windows) =>
                Results.Ok(new { examples = await windows.ExamplesAsync(http.RequestAborted) })
        );
    }
}

public sealed class SteeringWindows(NpgsqlDataSource db, DeedboxContext context)
{
    private const int TurnChars = 6000;

    private string Org => context.TenantId;

    private sealed record FactRow(
        string StreamId,
        string InterventionId,
        string Signal,
        string Phase,
        string? RuleIntent,
        string Repo,
        string? SessionId,
        int? Number,
        string? Text,
        string Refs,
        string? Intent,
        string? WentWrong,
        string? WentWrongLabel,
        string? Prevention
    );

    private const string FactColumns =
        "stream_id, intervention_id, signal, phase, rule_intent, repo, session_id, number, text, refs::text AS refs, intent, went_wrong, went_wrong_label, prevention";

    public async Task<IReadOnlyList<SteeringWorkerEndpoints.Window>> ForAsync(
        string stream,
        IReadOnlyList<string> ids,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var facts = await connection.QueryAsync<FactRow>(
            new CommandDefinition(
                $"SELECT {FactColumns} FROM casebox.steering_facts WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = ANY(@Ids) ORDER BY intervention_id",
                new
                {
                    Org,
                    Stream = stream,
                    Ids = ids.ToArray(),
                },
                cancellationToken: ct
            )
        );
        var result = new List<SteeringWorkerEndpoints.Window>();
        foreach (var f in facts)
            result.Add(await BuildAsync(connection, f, ct));
        return result;
    }

    public async Task<IReadOnlyList<SteeringWorkerEndpoints.Example>> ExamplesAsync(
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var facts = await connection.QueryAsync<FactRow>(
            new CommandDefinition(
                $"SELECT {FactColumns} FROM casebox.steering_facts WHERE org_id = @Org AND label_source = 'human' ORDER BY relabeled_at DESC LIMIT 20",
                new { Org },
                cancellationToken: ct
            )
        );
        var result = new List<SteeringWorkerEndpoints.Example>();
        foreach (var f in facts)
            result.Add(
                new SteeringWorkerEndpoints.Example(
                    await BuildAsync(connection, f, ct),
                    f.Intent!,
                    f.WentWrong,
                    f.WentWrongLabel,
                    f.Prevention
                )
            );
        return result;
    }

    // The task a session worked on: its work item's title, or its first prompt.
    public async Task<SteeringWorkerEndpoints.TaskText?> TaskAsync(
        string sessionId,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var text = await connection.QueryFirstOrDefaultAsync<string?>(
            new CommandDefinition(
                """
                SELECT coalesce(
                    (SELECT w.title || coalesce(E'\n\n' || (w.snapshot->>'description'), '') FROM casebox.sessions s JOIN casebox.work_items w ON w.org_id = s.org_id AND w.id = s.work_item_id
                     WHERE s.org_id = @Org AND s.id = @Id),
                    (SELECT text FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id AND kind = 'prompt' AND text IS NOT NULL ORDER BY at, seq LIMIT 1))
                """,
                new { Org, Id = sessionId },
                cancellationToken: ct
            )
        );
        return new SteeringWorkerEndpoints.TaskText(sessionId, Clip(Masking.Mask(text), TurnChars));
    }

    private sealed record EventRow(long Seq, DateTime At, string Kind, string? Text, string? Tool);

    private async Task<SteeringWorkerEndpoints.Window> BuildAsync(
        NpgsqlConnection connection,
        FactRow f,
        CancellationToken ct
    )
    {
        var refs =
            JsonSerializer.Deserialize<SteeringRefs>(f.Refs, JsonSerializerOptions.Web)
            ?? new SteeringRefs();
        var empty = new SteeringWorkerEndpoints.Turn(null, []);
        var (before, after, files) = (empty, empty, (IReadOnlyList<string>)[]);
        string? agent = null,
            model = null;

        if (f.SessionId is { } sessionId)
        {
            (agent, model) = await connection.QuerySingleOrDefaultAsync<(string?, string?)>(
                new CommandDefinition(
                    "SELECT agent, model FROM casebox.sessions WHERE org_id = @Org AND id = @Id",
                    new { Org, Id = sessionId },
                    cancellationToken: ct
                )
            );
            var events = await EventsAsync(connection, sessionId, ct);
            if (refs.RestartedBy is { } next)
            {
                before = LastTurn(events, events.Count);
                after = FirstTurn(await EventsAsync(connection, next, ct), -1);
            }
            else if (refs.Seqs is { Count: > 0 } seqs)
            {
                var first = events.FindIndex(e => e.Seq == seqs[0]);
                var last = events.FindLastIndex(e => seqs.Contains(e.Seq));
                if (first >= 0)
                {
                    before = LastTurn(events, first);
                    after = FirstTurn(events, last);
                    files = events
                        .Where(e => seqs.Contains(e.Seq))
                        .SelectMany(e => ToolOf(e)?.Files ?? [])
                        .Distinct()
                        .ToList();
                }
            }
        }
        else if (f.Number is { } number)
        {
            (agent, model) = await connection.QueryFirstOrDefaultAsync<(string?, string?)>(
                new CommandDefinition(
                    """
                    SELECT s.agent, s.model FROM casebox.sessions s JOIN casebox.pull_requests p ON p.org_id = s.org_id AND p.repo = s.repo AND p.head_ref = s.branch
                    WHERE p.org_id = @Org AND p.repo = @Repo AND p.number = @Number ORDER BY s.started_at LIMIT 1
                    """,
                    new
                    {
                        Org,
                        f.Repo,
                        Number = number,
                    },
                    cancellationToken: ct
                )
            );
        }

        return new SteeringWorkerEndpoints.Window(
            f.InterventionId,
            f.Signal,
            f.Phase,
            f.RuleIntent,
            agent,
            model,
            Masking.Mask(f.Text),
            before,
            after,
            files,
            f.Repo,
            refs.Commits ?? []
        );
    }

    private async Task<List<EventRow>> EventsAsync(
        NpgsqlConnection connection,
        string sessionId,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<EventRow>(
                new CommandDefinition(
                    """
                    SELECT seq, at, kind, text, tool::text AS tool FROM casebox.session_events
                    WHERE org_id = @Org AND session_id = @Id AND (seq < @Otel OR seq >= @Entire)
                      AND kind IN ('prompt', 'response', 'tool_call', 'tool_result', 'interruption', 'denial', 'rewind', 'human_edit')
                    ORDER BY at, seq
                    """,
                    new
                    {
                        Org,
                        Id = sessionId,
                        Otel = InSession.OtelSeqBase,
                        Entire = InSession.EntireSeqBase,
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();

    private static bool Agentic(EventRow e) => e.Kind is "response" or "tool_call" or "tool_result";

    // The agent's turn that ends just before index `end`.
    private static SteeringWorkerEndpoints.Turn LastTurn(List<EventRow> events, int end)
    {
        var start = end;
        while (start > 0 && Agentic(events[start - 1]))
            start--;
        return TurnOf(events.GetRange(start, end - start));
    }

    // The agent's turn that starts just after index `after`.
    private static SteeringWorkerEndpoints.Turn FirstTurn(List<EventRow> events, int after)
    {
        var end = after + 1;
        while (end < events.Count && Agentic(events[end]))
            end++;
        return TurnOf(events.GetRange(after + 1, end - after - 1));
    }

    private static SteeringWorkerEndpoints.Turn TurnOf(List<EventRow> turn)
    {
        var text = string.Join(
            "\n\n",
            turn.Where(e => e.Kind == "response" && !string.IsNullOrWhiteSpace(e.Text))
                .Select(e => e.Text)
        );
        var tools = turn.Where(e => e.Kind is "tool_call" or "tool_result")
            .Select(ToolOf)
            .OfType<SteeringWorkerEndpoints.ToolUse>()
            .TakeLast(40)
            .ToList();
        return new SteeringWorkerEndpoints.Turn(
            text.Length == 0 ? null : Clip(Masking.Mask(text), TurnChars),
            tools
        );
    }

    private static SteeringWorkerEndpoints.ToolUse? ToolOf(EventRow e) =>
        e.Tool is null
            ? null
            : JsonSerializer.Deserialize<SteeringWorkerEndpoints.ToolUse>(
                e.Tool,
                JsonSerializerOptions.Web
            );

    // Keeps the end of a long turn, where the agent says what it did.
    private static string? Clip(string? text, int max) =>
        text is null || text.Length <= max ? text : "…" + text[^max..];
}
