using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Cases;

public static class CaseJobs
{
    public const string Mine = "mine";
    public const string Validate = "case.validate";
    public const string Instruction = "case.instruction";

    public const int WindowDays = 183;
    public const int MaxSourceFiles = 12;
    public const int MaxCandidates = 300;

    // Corrections whose "what went wrong" a trace or a diff can check (SDD section 7).
    public static readonly string[] CheckableWentWrong =
    [
        "unverified_done",
        "wrong_area",
        "broke_convention",
    ];

    internal static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web)
    {
        Converters = { Infrastructure.CaseboxStreams.Enums },
    };
}

public sealed record CandidatePull(
    string Repo,
    int Number,
    string BaseRef,
    string BaseSha,
    string MergeSha,
    DateTimeOffset MergedAt,
    bool Bot
);

public sealed record CandidateCorrection(string Ref, string Signal, string WentWrong, string? Text);

public sealed record Candidate(
    string Key,
    CaseKind Kind,
    string? WorkItem,
    int Rank,
    IReadOnlyList<CandidatePull> Pulls,
    CandidatePull? Original,
    IReadOnlyList<CandidatePull> Related,
    string? Session,
    string? Base,
    CandidateCorrection? Correction
);

// Lists a workspace's candidates per repository and enqueues one mine job for each
// (docs/specs/cases.md, Mining). Only a workspace with a confirmed recipe is mined.
public sealed class CaseMining(
    NpgsqlDataSource db,
    IEventStore store,
    DeedboxContext context,
    JobQueue jobs,
    TimeProvider clock
)
{
    private string Org => context.TenantId;

    public async Task<int> EnqueueAsync(string workspaceName, CancellationToken ct)
    {
        var (workspace, _) = await store.Load<Workspace>(Workspace.StreamIdFor(workspaceName), ct);
        if (!workspace.Exists)
            throw new NotFoundException("The workspace does not exist.");
        if (!workspace.CanMine)
            throw new DomainException(
                "Cases are mined only in a workspace with a confirmed recipe. Run casebox env check, then casebox env confirm."
            );

        await using var connection = await db.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var now = clock.GetUtcNow();
        var enqueued = 0;
        foreach (var repo in workspace.Repos)
        {
            var candidates = await CandidatesAsync(
                connection,
                transaction,
                workspace,
                repo,
                now,
                ct
            );
            var payload = new
            {
                workspace = workspace.Name,
                repo,
                recipeHash = workspace.RecipeHash,
                windowDays = CaseJobs.WindowDays,
                maxSourceFiles = CaseJobs.MaxSourceFiles,
                candidates,
            };
            await jobs.EnqueueAsync(
                transaction,
                Org,
                CaseJobs.Mine,
                $"{CaseJobs.Mine}:{workspace.Name}:{repo}:{Ids.New()}",
                JsonSerializer.SerializeToElement(payload, CaseJobs.Json),
                3,
                ct
            );
            enqueued++;
        }

        await transaction.CommitAsync(ct);
        return enqueued;
    }

    private sealed record PullRow(
        string Repo,
        int Number,
        string? WorkItemId,
        string? LinkConfidence,
        string Snapshot,
        DateTime MergedAt,
        string MergeSha
    );

    private async Task<List<Candidate>> CandidatesAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction transaction,
        Workspace workspace,
        string repo,
        DateTimeOffset now,
        CancellationToken ct
    )
    {
        var since = now.AddDays(-CaseJobs.WindowDays);
        var pulls = (
            await connection.QueryAsync<PullRow>(
                new CommandDefinition(
                    """
                    SELECT p.repo, p.number, p.work_item_id, (SELECT max(link_confidence) FROM casebox.sessions s WHERE s.org_id = p.org_id AND s.work_item_id = p.work_item_id) AS link_confidence,
                           p.snapshot::text AS snapshot, p.merged_at, p.merge_sha
                    FROM casebox.pull_requests p
                    WHERE p.org_id = @Org AND p.merged_at >= @Since AND p.merge_sha IS NOT NULL AND (p.repo = @Repo OR p.work_item_id IN (
                        SELECT work_item_id FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND work_item_id IS NOT NULL))
                    """,
                    new
                    {
                        Org,
                        Repo = repo,
                        Since = since,
                    },
                    transaction,
                    cancellationToken: ct
                )
            )
        ).ToList();

        CandidatePull Pull(PullRow p)
        {
            var snapshot = JsonSerializer.Deserialize<PullRequestSnapshot>(
                p.Snapshot,
                GitHubJson.Options
            )!;
            return new CandidatePull(
                p.Repo,
                p.Number,
                snapshot.BaseRef,
                snapshot.BaseSha,
                p.MergeSha,
                new DateTimeOffset(DateTime.SpecifyKind(p.MergedAt, DateTimeKind.Utc)),
                snapshot.Author?.Bot ?? false
            );
        }

        int Rank(string? workItem, DateTimeOffset merged) =>
            (workItem is null ? 2000 : 10000) - (int)(now - merged).TotalDays;

        IReadOnlyList<CandidatePull> Related(string? workItem, string except) =>
            workItem is null
                ? []
                : pulls
                    .Where(p =>
                        p.WorkItemId == workItem
                        && $"{p.Repo}#{p.Number}" != except
                        && workspace.Repos.Contains(p.Repo)
                    )
                    .Select(Pull)
                    .ToList();

        var candidates = new List<Candidate>();
        foreach (var p in pulls.Where(p => p.Repo == repo))
        {
            var snapshot = JsonSerializer.Deserialize<PullRequestSnapshot>(
                p.Snapshot,
                GitHubJson.Options
            )!;
            if (snapshot.Reverts is not null || snapshot.Author is { Bot: true })
                continue;
            var pull = Pull(p);
            candidates.Add(
                new Candidate(
                    $"pr:{repo}#{p.Number}",
                    CaseKind.Capability,
                    p.WorkItemId,
                    Rank(p.WorkItemId, pull.MergedAt),
                    [pull],
                    null,
                    Related(p.WorkItemId, $"{repo}#{p.Number}"),
                    null,
                    null,
                    null
                )
            );
        }

        // A fix of an earlier pull request: the fix's tests plus the original's form the oracle.
        var fixes = await connection.QueryAsync<(string InterventionId, int Number)>(
            new CommandDefinition(
                """
                SELECT intervention_id, number FROM casebox.steering_facts
                WHERE org_id = @Org AND repo = @Repo AND signal = 'fix' AND intervention_id LIKE 'fix:pr:%' AND at >= @Since
                """,
                new
                {
                    Org,
                    Repo = repo,
                    Since = since,
                },
                transaction,
                cancellationToken: ct
            )
        );
        foreach (var (interventionId, number) in fixes)
        {
            var fixer = interventionId["fix:pr:".Length..];
            var hash = fixer.LastIndexOf('#');
            if (hash < 0 || !int.TryParse(fixer[(hash + 1)..], out var fixNumber))
                continue;
            var fix = pulls.FirstOrDefault(p => p.Repo == fixer[..hash] && p.Number == fixNumber);
            var original = pulls.FirstOrDefault(p => p.Repo == repo && p.Number == number);
            if (fix is null || original is null || fix.Repo != repo)
                continue;
            var fixPull = Pull(fix);
            candidates.Add(
                new Candidate(
                    $"fix:{repo}#{fixNumber}",
                    CaseKind.Regression,
                    original.WorkItemId,
                    Rank(original.WorkItemId, fixPull.MergedAt) + 500,
                    [fixPull],
                    Pull(original),
                    Related(original.WorkItemId, $"{repo}#{number}"),
                    null,
                    null,
                    null
                )
            );
        }

        // Corrections a trace or a diff can check, in sessions whose base commit is known.
        var corrections = await connection.QueryAsync<(
            string Ref,
            string Signal,
            string WentWrong,
            string? Text,
            string SessionId,
            string Base,
            string? WorkItemId,
            DateTime At
        )>(
            new CommandDefinition(
                """
                SELECT f.ref, f.signal, f.went_wrong, f.text, s.id, coalesce(s.head_start, s.base_commit), s.work_item_id, f.at
                FROM casebox.steering_facts f JOIN casebox.sessions s ON s.org_id = f.org_id AND s.id = f.session_id
                WHERE f.org_id = @Org AND s.repo = @Repo AND f.intent = 'correction' AND f.went_wrong = ANY(@WentWrong)
                  AND coalesce(s.head_start, s.base_commit) IS NOT NULL AND f.at >= @Since
                ORDER BY f.at DESC LIMIT @Max
                """,
                new
                {
                    Org,
                    Repo = repo,
                    WentWrong = CaseJobs.CheckableWentWrong,
                    Since = since,
                    Max = CaseJobs.MaxCandidates,
                },
                transaction,
                cancellationToken: ct
            )
        );
        foreach (var c in corrections)
        {
            var itemPulls = c.WorkItemId is null
                ? []
                : pulls
                    .Where(p => p.WorkItemId == c.WorkItemId && p.Repo == repo)
                    .Select(Pull)
                    .ToList();
            var at = new DateTimeOffset(DateTime.SpecifyKind(c.At, DateTimeKind.Utc));
            candidates.Add(
                new Candidate(
                    $"steering:{c.Ref}",
                    CaseKind.Steering,
                    c.WorkItemId,
                    Rank(c.WorkItemId, at) + 200,
                    itemPulls,
                    null,
                    Related(c.WorkItemId, ""),
                    c.SessionId,
                    c.Base,
                    new CandidateCorrection(c.Ref, c.Signal, c.WentWrong, Masking.Mask(c.Text))
                )
            );
        }

        return candidates
            .OrderByDescending(c => c.Rank)
            .ThenBy(c => c.Key, StringComparer.Ordinal)
            .Take(CaseJobs.MaxCandidates)
            .ToList();
    }
}

// The worker's answer to a mine job.
public sealed record MinedCase(
    string Key,
    CaseKind Kind,
    CaseScope Scope,
    IReadOnlyList<CaseRepo> Repos,
    string HarnessHash,
    int Rank
);

public sealed record MineResult(IReadOnlyList<MinedCase>? Cases);

public sealed class MineResultHandler(IServiceProvider services) : IJobResultHandler
{
    public string Kind => CaseJobs.Mine;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var jobs = services.GetRequiredService<JobQueue>();
        var payload = result.Job.Payload;
        var workspaceName = payload.GetProperty("workspace").GetString()!;
        var candidates = payload
            .GetProperty("candidates")
            .Deserialize<List<Candidate>>(CaseJobs.Json)!
            .ToDictionary(c => c.Key, StringComparer.Ordinal);
        var answer = result.Result.Deserialize<MineResult>(CaseJobs.Json);
        var (workspace, _) = await result.Store.Load<Workspace>(
            Workspace.StreamIdFor(workspaceName),
            ct
        );
        if (!workspace.CanMine)
            return;

        foreach (var mined in answer?.Cases ?? [])
        {
            if (
                !candidates.TryGetValue(mined.Key, out var candidate)
                || mined.Kind != candidate.Kind
            )
                continue;
            var id = Case.IdFor(result.OrgId, mined.Key);
            var appended = await result.Store.Execute<Case>(
                Case.StreamId(id),
                c =>
                    CaseDecider.Mine(
                        c,
                        new CaseEvents.Mined(
                            mined.Kind,
                            workspace.Name,
                            mined.Key,
                            candidate.WorkItem,
                            mined.Scope,
                            mined.Repos,
                            workspace.RecipeHash!,
                            mined.HarnessHash,
                            mined.Rank
                        )
                    ),
                ct
            );
            if (appended.Events.Count == 0)
                continue;
            await jobs.EnqueueAsync(
                result.Transaction,
                result.OrgId,
                CaseJobs.Validate,
                $"{CaseJobs.Validate}:{id}:{workspace.RecipeHash}",
                ValidatePayload(id, mined, candidate, workspace),
                3,
                ct
            );
        }
    }

    internal static JsonElement ValidatePayload(
        string id,
        MinedCase mined,
        Candidate candidate,
        Workspace workspace
    ) =>
        JsonSerializer.SerializeToElement(
            new
            {
                caseId = id,
                kind = mined.Kind,
                workspace = workspace.Name,
                recipe = JsonDocument.Parse(workspace.Recipe!).RootElement,
                recipeHash = workspace.RecipeHash,
                repos = mined.Repos,
                pulls = candidate.Pulls,
                original = candidate.Original,
                correction = candidate.Correction,
                session = candidate.Session,
            },
            CaseJobs.Json
        );
}

public sealed record ValidateResult(
    bool Passed,
    string? Reason,
    string? Detail,
    string? Oracle,
    int FailToPass,
    int PassToPass,
    double Seconds,
    bool Drift,
    double Weight,
    bool Retire
);

public sealed class ValidateResultHandler(IServiceProvider services) : IJobResultHandler
{
    public string Kind => CaseJobs.Validate;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var id = result.Job.Payload.GetProperty("caseId").GetString()!;
        var answer =
            result.Result.Deserialize<ValidateResult>(CaseJobs.Json)
            ?? throw new DomainException("The result is empty.");
        var stream = Case.StreamId(id);
        if (!answer.Passed)
        {
            var reason =
                SteeringFacts.Parse<ValidationFailure>(answer.Reason)
                ?? throw new DomainException($"'{answer.Reason}' is not a validation failure.");
            await result.Store.Execute<Case>(
                stream,
                c =>
                    CaseDecider.FailValidation(
                        c,
                        new CaseEvents.ValidationFailed(reason, Clip(answer.Detail))
                    ),
                ct
            );
            return;
        }

        if (answer.Oracle is null)
            throw new DomainException("A passed validation names its oracle blob.");
        if (answer.Retire)
        {
            await result.Store.Execute<Case>(
                stream,
                c => CaseDecider.Retire(c, RetireReason.Drift),
                ct
            );
            return;
        }

        await result.Store.Execute<Case>(
            stream,
            c =>
                CaseDecider.Validate(
                    c,
                    new CaseEvents.Validated(
                        answer.Oracle,
                        answer.FailToPass,
                        answer.PassToPass,
                        answer.Seconds,
                        answer.Drift,
                        answer.Drift ? 0.5 : 1
                    )
                ),
            ct
        );
        var (state, _) = await result.Store.Load<Case>(stream, ct);
        if (state.HasInstruction)
            return;
        var payload = result.Job.Payload;
        await services
            .GetRequiredService<JobQueue>()
            .EnqueueAsync(
                result.Transaction,
                result.OrgId,
                CaseJobs.Instruction,
                $"{CaseJobs.Instruction}:{id}:{answer.Oracle}",
                JsonSerializer.SerializeToElement(
                    new
                    {
                        caseId = id,
                        kind = payload.GetProperty("kind"),
                        repos = payload.GetProperty("repos"),
                        oracle = answer.Oracle,
                    },
                    CaseJobs.Json
                ),
                3,
                ct
            );
    }

    private static string? Clip(string? text) => text is { Length: > 4000 } ? text[..4000] : text;
}

public sealed record InstructionResult(
    string? Text,
    IReadOnlyList<string>? Signatures,
    IReadOnlyList<Assertion>? Assertions,
    IReadOnlyList<JudgeQuestion>? Judge,
    string? Model
);

public sealed class InstructionResultHandler : IJobResultHandler
{
    public string Kind => CaseJobs.Instruction;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var id = result.Job.Payload.GetProperty("caseId").GetString()!;
        var answer =
            result.Result.Deserialize<InstructionResult>(CaseJobs.Json)
            ?? throw new DomainException("The result is empty.");
        var person = await CaseSources.PersonAsync(
            result.Transaction.Connection!,
            result.Transaction,
            result.OrgId,
            id,
            ct
        );
        await result.Store.Execute<Case>(
            Case.StreamId(id),
            c =>
                c.HasInstruction
                    ? []
                    : CaseDecider.DraftInstruction(
                        c,
                        new CaseEvents.InstructionDrafted(
                            answer.Text?.Trim(),
                            person,
                            answer.Signatures ?? [],
                            answer.Assertions ?? [],
                            answer.Judge ?? [],
                            answer.Model ?? "unknown"
                        )
                    ),
            ct
        );
    }
}

// What an instruction is written from: the work item's snapshot, or the steering window. Person
// tokens are masked; the model never sees who anyone is.
public static class CaseSources
{
    // The person whose words the instruction may carry: the work item's assignee, or the corrected
    // session's person; "system" when there is none.
    public static async Task<string> PersonAsync(
        System.Data.Common.DbConnection connection,
        System.Data.Common.DbTransaction? transaction,
        string org,
        string id,
        CancellationToken ct
    ) =>
        await connection.QuerySingleOrDefaultAsync<string?>(
            new CommandDefinition(
                """
                SELECT coalesce(
                    (SELECT f.person FROM casebox.steering_facts f WHERE f.org_id = c.org_id AND c.source = 'steering:' || f.ref),
                    (SELECT w.snapshot->>'assignee' FROM casebox.work_items w WHERE w.org_id = c.org_id AND w.id = c.work_item))
                FROM casebox.case_catalog c WHERE c.org_id = @Org AND c.id = @Id
                """,
                new { Org = org, Id = id },
                transaction,
                cancellationToken: ct
            )
        )
            is { Length: > 0 } person
            ? person
            : "system";

    public static void MapCaseWorker(this RouteGroupBuilder worker)
    {
        worker
            .MapGet(
                "/cases/{id}/source",
                async (string id, HttpContext http, NpgsqlDataSource db, SteeringWindows windows) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    var c = await connection.QuerySingleOrDefaultAsync<(
                        string Kind,
                        string Source,
                        string? WorkItem
                    )?>(
                        new CommandDefinition(
                            "SELECT kind, source, work_item FROM casebox.case_catalog WHERE org_id = @Org AND id = @Id",
                            new { Org = http.User.OrgId(), Id = id },
                            cancellationToken: http.RequestAborted
                        )
                    );
                    if (c is null)
                        return Results.NotFound();

                    var item = c.Value.WorkItem is null
                        ? null
                        : await connection.QuerySingleOrDefaultAsync<(
                            string Title,
                            string? Description,
                            string? Type
                        )?>(
                            new CommandDefinition(
                                "SELECT title, snapshot->>'description', type FROM casebox.work_items WHERE org_id = @Org AND id = @Id",
                                new { Org = http.User.OrgId(), Id = c.Value.WorkItem },
                                cancellationToken: http.RequestAborted
                            )
                        );
                    string? pullTitle = null;
                    if (
                        c.Value.Source.StartsWith("pr:", StringComparison.Ordinal)
                        || c.Value.Source.StartsWith("fix:", StringComparison.Ordinal)
                    )
                    {
                        var pr = c.Value.Source[(c.Value.Source.IndexOf(':') + 1)..];
                        var hash = pr.LastIndexOf('#');
                        pullTitle = await connection.QuerySingleOrDefaultAsync<string?>(
                            new CommandDefinition(
                                "SELECT snapshot->>'title' FROM casebox.pull_requests WHERE org_id = @Org AND repo = @Repo AND number = @Number",
                                new
                                {
                                    Org = http.User.OrgId(),
                                    Repo = pr[..hash],
                                    Number = int.Parse(
                                        pr[(hash + 1)..],
                                        System.Globalization.CultureInfo.InvariantCulture
                                    ),
                                },
                                cancellationToken: http.RequestAborted
                            )
                        );
                    }

                    object? steering = null;
                    if (c.Value.Source.StartsWith("steering:", StringComparison.Ordinal))
                    {
                        var fact = await connection.QuerySingleOrDefaultAsync<(
                            string Stream,
                            string Intervention,
                            string SessionId
                        )?>(
                            new CommandDefinition(
                                "SELECT stream_id, intervention_id, session_id FROM casebox.steering_facts WHERE org_id = @Org AND ref = @Ref",
                                new
                                {
                                    Org = http.User.OrgId(),
                                    Ref = c.Value.Source["steering:".Length..],
                                },
                                cancellationToken: http.RequestAborted
                            )
                        );
                        if (fact is { } f)
                        {
                            var prompts = await connection.QueryAsync<string>(
                                new CommandDefinition(
                                    """
                                    SELECT text FROM casebox.session_events
                                    WHERE org_id = @Org AND session_id = @Session AND kind = 'prompt' AND text IS NOT NULL
                                      AND at < (SELECT at FROM casebox.steering_facts WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = @Intervention)
                                    ORDER BY at, seq
                                    """,
                                    new
                                    {
                                        Org = http.User.OrgId(),
                                        Session = f.SessionId,
                                        f.Stream,
                                        f.Intervention,
                                    },
                                    cancellationToken: http.RequestAborted
                                )
                            );
                            steering = new
                            {
                                prompts = prompts.Select(Masking.Mask).ToList(),
                                window = (
                                    await windows.ForAsync(
                                        f.Stream,
                                        [f.Intervention],
                                        http.RequestAborted
                                    )
                                ).FirstOrDefault(),
                            };
                        }
                    }

                    return Results.Ok(
                        new
                        {
                            kind = c.Value.Kind,
                            workItem = item is { } i
                                ? new
                                {
                                    title = Masking.Mask(i.Title),
                                    description = Masking.Mask(i.Description),
                                    type = i.Type,
                                }
                                : null,
                            pullTitle = Masking.Mask(pullTitle),
                            steering,
                        }
                    );
                }
            )
            .WithTags("Worker")
            .RequireAuthorization(Policies.Worker);
    }
}
