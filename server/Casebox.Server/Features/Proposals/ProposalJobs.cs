using System.Data.Common;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

// proposal.draft (docs/specs/simple-evolution.md): one draft per pattern, at most MaxPending
// proposals waiting for a person per workspace, drafted by a worker's analysis model from the
// pattern's corrections and the repository's current harness files.
public static class ProposalJobs
{
    public const string Draft = "proposal.draft";
    public const int MaxPending = 3;
    public static readonly TimeSpan RejectionMemory = TimeSpan.FromDays(90);

    // Queues a draft for the biggest patterns without a proposal, while the workspace has room. A
    // pattern gets a new draft only when new corrections join it: the job key holds its size.
    public static async Task EnqueueDraftsAsync(
        NpgsqlConnection connection,
        JobQueue jobs,
        string org,
        IReadOnlyDictionary<string, HashSet<string>> workspaces,
        CancellationToken ct
    )
    {
        foreach (var workspace in workspaces.Keys)
        {
            var args = new
            {
                Org = org,
                Workspace = workspace,
                Kind = Draft,
            };
            var pending = await connection.ExecuteScalarAsync<int>(
                new CommandDefinition(
                    """
                    SELECT (SELECT count(*) FROM casebox.proposals WHERE org_id = @Org AND workspace = @Workspace AND status IN ('open', 'approved'))
                         + (SELECT count(*) FROM casebox.jobs WHERE org_id = @Org AND kind = @Kind AND status IN ('queued', 'leased') AND payload->>'workspace' = @Workspace)
                    """,
                    args,
                    cancellationToken: ct
                )
            );
            if (pending >= MaxPending)
                continue;
            var patterns = await connection.QueryAsync<(string Id, int Size, string Refs)>(
                new CommandDefinition(
                    """
                    SELECT p.id, jsonb_array_length(p.refs), p.refs::text FROM casebox.patterns p
                    WHERE p.org_id = @Org AND p.workspace = @Workspace AND p.status IN ('open', 'acknowledged') AND NOT p.advisory
                      AND NOT EXISTS (SELECT 1 FROM casebox.proposals x WHERE x.org_id = p.org_id AND x.pattern = p.id AND x.status IN ('open', 'approved', 'applied'))
                      AND NOT EXISTS (SELECT 1 FROM casebox.jobs j WHERE j.org_id = p.org_id AND j.kind = @Kind AND j.status IN ('queued', 'leased') AND j.payload->>'pattern' = p.id)
                    ORDER BY jsonb_array_length(p.refs) DESC, p.id
                    """,
                    args,
                    cancellationToken: ct
                )
            );
            foreach (var p in patterns.Take(MaxPending - pending))
            {
                var repo = await connection.QuerySingleOrDefaultAsync<string>(
                    new CommandDefinition(
                        """
                        SELECT repo FROM casebox.steering_facts WHERE org_id = @Org AND ref = ANY(@Refs)
                        GROUP BY repo ORDER BY count(*) DESC, repo LIMIT 1
                        """,
                        new { Org = org, Refs = JsonSerializer.Deserialize<string[]>(p.Refs) },
                        cancellationToken: ct
                    )
                );
                if (repo is null)
                    continue;
                await using var transaction = await connection.BeginTransactionAsync(ct);
                await jobs.EnqueueAsync(
                    transaction,
                    org,
                    Draft,
                    $"{Draft}:{p.Id}:{p.Size}",
                    new
                    {
                        pattern = p.Id,
                        workspace,
                        repo,
                    },
                    3,
                    ct
                );
                await transaction.CommitAsync(ct);
            }
        }
    }

    // The hash of what a proposal changes, so a rejected change is not drafted again.
    public static string ContentHash(
        ProposalKind kind,
        IReadOnlyList<Edit> edits,
        CodeNote? note
    ) =>
        Convert.ToHexStringLower(
            SHA256.HashData(
                Encoding.UTF8.GetBytes(
                    JsonSerializer.Serialize(
                        new
                        {
                            kind = ProposalBoard.KindName(kind),
                            edits,
                            note = note?.What,
                        },
                        ProposalBoard.Json
                    )
                )
            )
        );
}

// The worker's answer: a draft, or a note that no repository change prevents the pattern.
public sealed record DraftAnswer(
    string? Advisory,
    ProposalKind? Kind,
    string? Title,
    string? Rationale,
    IReadOnlyList<Edit>? Edits,
    IReadOnlyList<FilePreview>? Preview,
    CodeNote? Note,
    string? BaseCommit,
    string? Model
);

public sealed class DraftResultHandler : IJobResultHandler
{
    public string Kind => ProposalJobs.Draft;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var payload = result.Job.Payload;
        var patternId = payload.GetProperty("pattern").GetString()!;
        var workspace = payload.GetProperty("workspace").GetString()!;
        var repo = payload.GetProperty("repo").GetString()!;
        var answer =
            result.Result.Deserialize<DraftAnswer>(ProposalBoard.Json)
            ?? throw new DomainException("The result is empty.");

        if (answer.Advisory is { } note)
        {
            await result.Store.Execute<Pattern>(
                Pattern.StreamId(patternId),
                p => PatternDecider.NoteAdvisory(p, note),
                ct
            );
            return;
        }

        var kind = answer.Kind ?? throw new DomainException("The draft names no kind.");
        var edits = answer.Edits ?? [];
        var hash = ProposalJobs.ContentHash(kind, edits, answer.Note);
        var connection = result.Transaction.Connection!;
        var rejectedBefore = await connection.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                """
                SELECT EXISTS (SELECT 1 FROM casebox.proposals WHERE org_id = @Org AND content_hash = @Hash AND status = 'rejected' AND updated_at >= @Since)
                """,
                new
                {
                    Org = result.OrgId,
                    Hash = hash,
                    Since = DateTimeOffset.UtcNow - ProposalJobs.RejectionMemory,
                },
                result.Transaction,
                cancellationToken: ct
            )
        );
        if (rejectedBefore)
            return;

        var drafted = new ProposalEvents.Drafted(
            patternId,
            workspace,
            repo,
            kind,
            Clip(answer.Title ?? "", 200),
            Clip(answer.Rationale ?? "", 1000),
            edits,
            answer.Preview ?? [],
            answer.Note,
            hash,
            answer.BaseCommit ?? "",
            answer.Model
        );
        await result.Store.Execute<Proposal>(
            Proposal.StreamId(Ids.New()),
            p => ProposalDecider.Draft(p, drafted),
            ct
        );
    }

    private static string Clip(string s, int max) => s.Length <= max ? s : s[..max] + "…";
}

public static class ProposalWorkerEndpoints
{
    public sealed record Evidence(
        object Pattern,
        IReadOnlyList<SteeringWorkerEndpoints.Window> Windows,
        IReadOnlyList<object> Rejections
    );

    // What a draft may read: the pattern's windows (masked) and the rejections of the last 90 days
    // for the pattern or its workspace, with their reasons.
    public static void MapProposalWorker(this RouteGroupBuilder worker)
    {
        worker
            .MapGet(
                "/patterns/{id}/evidence",
                async (
                    string id,
                    HttpContext http,
                    NpgsqlDataSource db,
                    IEventStore store,
                    SteeringWindows windows,
                    TimeProvider clock
                ) =>
                {
                    var ct = http.RequestAborted;
                    var org = http.User.OrgId();
                    var (p, _) = await store.Load<Pattern>(Pattern.StreamId(id), ct);
                    if (!p.Exists)
                        return Results.NotFound();
                    await using var connection = await db.OpenConnectionAsync(ct);
                    var facts = await connection.QueryAsync<(string Stream, string Intervention)>(
                        new CommandDefinition(
                            "SELECT stream_id, intervention_id FROM casebox.steering_facts WHERE org_id = @Org AND ref = ANY(@Refs) ORDER BY at DESC LIMIT 30",
                            new { Org = org, Refs = p.Refs.ToArray() },
                            cancellationToken: ct
                        )
                    );
                    var found = new List<SteeringWorkerEndpoints.Window>();
                    foreach (var g in facts.GroupBy(f => f.Stream))
                        found.AddRange(
                            await windows.ForAsync(g.Key, [.. g.Select(f => f.Intervention)], ct)
                        );
                    var rejections = (
                        await connection.QueryAsync<(
                            string Title,
                            string Edits,
                            string? Note,
                            string? Reason
                        )>(
                            new CommandDefinition(
                                """
                                SELECT title, edits::text, note::text, reason FROM casebox.proposals
                                WHERE org_id = @Org AND status = 'rejected' AND updated_at >= @Since AND (pattern = @Pattern OR workspace = @Workspace)
                                ORDER BY updated_at DESC LIMIT 20
                                """,
                                new
                                {
                                    Org = org,
                                    Since = clock.GetUtcNow() - ProposalJobs.RejectionMemory,
                                    Pattern = id,
                                    p.Workspace,
                                },
                                cancellationToken: ct
                            )
                        )
                    )
                        .Select(r =>
                            (object)
                                new
                                {
                                    r.Title,
                                    Edits = JsonDocument.Parse(r.Edits).RootElement,
                                    Note = r.Note is null
                                        ? (JsonElement?)null
                                        : JsonDocument.Parse(r.Note).RootElement,
                                    Reason = Masking.Mask(r.Reason),
                                }
                        )
                        .ToList();
                    var row = await connection.QuerySingleAsync<(string Title, string Summary)>(
                        new CommandDefinition(
                            "SELECT title, summary FROM casebox.patterns WHERE org_id = @Org AND id = @Id",
                            new { Org = org, Id = id },
                            cancellationToken: ct
                        )
                    );
                    return Results.Ok(
                        new Evidence(
                            new
                            {
                                id,
                                row.Title,
                                row.Summary,
                                p.Key!.WentWrong,
                                p.Key.Label,
                                p.Key.Prevention,
                                p.Key.Path,
                            },
                            found,
                            rejections
                        )
                    );
                }
            )
            .RequireAuthorization(Policies.Worker);
    }
}
