using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Patterns;

public static class PatternJobs
{
    public const string Cluster = "pattern.cluster";
}

// Runs the pattern scan once an hour for every organisation with corrections.
public sealed class PatternScheduler(
    IServiceScopeFactory scopes,
    NpgsqlDataSource db,
    TimeProvider clock,
    ILogger<PatternScheduler> logger
) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await RunAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogError(e, "The pattern scan failed; it runs again in an hour.");
            }

            await Task.Delay(TimeSpan.FromHours(1), clock, stoppingToken);
        }
    }

    public async Task RunAsync(CancellationToken ct)
    {
        List<string> orgs;
        await using (var connection = await db.OpenConnectionAsync(ct))
            orgs = (
                await connection.QueryAsync<string>(
                    new CommandDefinition(
                        "SELECT DISTINCT org_id FROM casebox.steering_facts WHERE intent = 'correction'",
                        cancellationToken: ct
                    )
                )
            ).ToList();
        foreach (var org in orgs)
        {
            await using var scope = scopes.CreateAsyncScope();
            var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
            context.TenantId = org;
            context.Metadata = new EventMetadata { Actor = "system:patterns" };
            await scope.ServiceProvider.GetRequiredService<PatternScan>().RunAsync(ct);
        }
    }
}

// One pass for the current organisation (docs/specs/self-evolution.md, Groups): each workspace's
// corrections of the last 60 days, grouped by what went wrong, prevention and top-level path; a
// group with 3 corrections from k people gets a pattern.cluster job for its current set.
public sealed class PatternScan(
    NpgsqlDataSource db,
    IEventStore store,
    DeedboxContext context,
    JobQueue jobs,
    SteeringWindows windows,
    Solo solo,
    TimeProvider clock
)
{
    public const int MinimumCorrections = 3;
    public const int MaxCorrections = 60;
    public static readonly TimeSpan Window = TimeSpan.FromDays(60);

    private string Org => context.TenantId;

    public sealed record Correction(
        string StreamId,
        string InterventionId,
        string Ref,
        string Repo,
        string Person,
        bool PersonMapped,
        string Period,
        string WentWrong,
        string? WentWrongLabel,
        string Prevention,
        DateTime At,
        string? Path
    );

    public async Task RunAsync(CancellationToken ct)
    {
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        var k = await solo.ViewAsync(org, ct);
        await using var connection = await db.OpenConnectionAsync(ct);
        var corrections = await CorrectionsAsync(connection, null, ct);
        await PathsAsync(connection, corrections, ct);
        corrections = await CorrectionsAsync(connection, null, ct);

        var workspaces = await WorkspacesAsync(connection, ct);
        foreach (var (workspace, repos) in workspaces)
        {
            var groups = corrections
                .Where(c => repos.Contains(c.Repo))
                .GroupBy(Key)
                .Where(g => Ready(g, k));
            foreach (var group in groups)
            {
                var set = group.OrderByDescending(c => c.At).Take(MaxCorrections).ToList();
                var setHash = Hash(
                    string.Join(",", set.Select(c => c.Ref).Order(StringComparer.Ordinal))
                );
                await using var transaction = await connection.BeginTransactionAsync(ct);
                await jobs.EnqueueAsync(
                    transaction,
                    Org,
                    PatternJobs.Cluster,
                    $"{PatternJobs.Cluster}:{workspace}:{group.Key.Hash}:{setHash}",
                    JsonSerializer.SerializeToElement(
                        new
                        {
                            workspace,
                            key = group.Key,
                            corrections = set.Select(c => new
                            {
                                @ref = c.Ref,
                                stream = c.StreamId,
                                interventionId = c.InterventionId,
                            }),
                        },
                        Evaluations.EvaluationResults.Json
                    ),
                    3,
                    ct
                );
                await transaction.CommitAsync(ct);
            }
        }

        await ObserveOutcomesAsync(connection, workspaces, ct);
    }

    public static readonly TimeSpan OutcomeWindow = TimeSpan.FromDays(30);

    // 30 days after a proposal's merge: the pattern group's corrections per 100 sessions of the
    // workspace in the 30 days before and after (observational). A fall resolves the pattern.
    private async Task ObserveOutcomesAsync(
        System.Data.Common.DbConnection connection,
        Dictionary<string, HashSet<string>> workspaces,
        CancellationToken ct
    )
    {
        var due = await connection.QueryAsync<(
            string Id,
            string Workspace,
            string Pattern,
            DateTime MergedAt
        )>(
            new CommandDefinition(
                """
                SELECT id, workspace, pattern, merged_at FROM casebox.proposals
                WHERE org_id = @Org AND status = 'merged' AND outcome IS NULL AND pattern IS NOT NULL AND merged_at <= @Due
                """,
                new { Org, Due = clock.GetUtcNow() - OutcomeWindow },
                cancellationToken: ct
            )
        );
        foreach (var d in due)
        {
            var (pattern, _) = await store.Load<Pattern>(Pattern.StreamId(d.Pattern), ct);
            if (!pattern.Exists || !workspaces.TryGetValue(d.Workspace, out var repos))
                continue;
            var merged = new DateTimeOffset(DateTime.SpecifyKind(d.MergedAt, DateTimeKind.Utc));
            async Task<(int Corrections, int Sessions)> CountAsync(
                DateTimeOffset from,
                DateTimeOffset to
            )
            {
                var key = pattern.Key!;
                var args = new
                {
                    Org,
                    Repos = repos.ToArray(),
                    From = from,
                    To = to,
                    key.WentWrong,
                    key.Label,
                    key.Prevention,
                    key.Path,
                };
                var corrections = await connection.ExecuteScalarAsync<int>(
                    new CommandDefinition(
                        """
                        SELECT count(*) FROM casebox.steering_facts f
                        LEFT JOIN casebox.correction_paths p ON p.org_id = f.org_id AND p.stream_id = f.stream_id AND p.intervention_id = f.intervention_id
                        WHERE f.org_id = @Org AND f.repo = ANY(@Repos) AND f.at >= @From AND f.at < @To AND f.intent = 'correction'
                          AND f.went_wrong = @WentWrong AND (@Label::text IS NULL OR f.went_wrong_label = @Label) AND f.prevention = @Prevention
                          AND coalesce(p.path, '*') = @Path
                        """,
                        args,
                        cancellationToken: ct
                    )
                );
                var sessions = await connection.ExecuteScalarAsync<int>(
                    new CommandDefinition(
                        "SELECT count(*) FROM casebox.sessions WHERE org_id = @Org AND repo = ANY(@Repos) AND started_at >= @From AND started_at < @To",
                        args,
                        cancellationToken: ct
                    )
                );
                return (corrections, sessions);
            }
            var before = await CountAsync(merged - OutcomeWindow, merged);
            var after = await CountAsync(merged, merged + OutcomeWindow);
            double Rate((int Corrections, int Sessions) c) =>
                c.Sessions == 0 ? 0 : 100.0 * c.Corrections / c.Sessions;
            var rateBefore = Math.Round(Rate(before), 2);
            var rateAfter = Math.Round(Rate(after), 2);
            var fell = before.Sessions > 0 && after.Sessions > 0 && rateAfter < rateBefore;
            var outcome = new Proposals.ProposalEvents.OutcomeObserved(
                rateBefore,
                rateAfter,
                before.Sessions,
                after.Sessions,
                fell
            );
            await store.Execute<Proposals.Proposal>(
                Proposals.Proposal.StreamId(d.Id),
                p => Proposals.ProposalDecider.Observe(p, outcome),
                ct
            );
            if (fell)
                await store.Execute<Pattern>(
                    Pattern.StreamId(d.Pattern),
                    p => PatternDecider.Resolve(p, d.Id, outcome.Before, outcome.After),
                    ct
                );
        }
    }

    public static PatternKey Key(Correction c) =>
        new(
            c.WentWrong,
            c.WentWrong == "other" ? c.WentWrongLabel : null,
            c.Prevention,
            c.Path ?? "*"
        );

    // At least 3 corrections from at least k mapped people (per pseudonym period).
    public static bool Ready(IEnumerable<Correction> corrections, KView k)
    {
        var list = corrections.ToList();
        return list.Count >= MinimumCorrections
            && KRule.Meets(list.Select(c => new Person(c.Person, c.PersonMapped, c.Period)), k);
    }

    // The workspace's corrections of the last 60 days; refs limits them to those refs.
    public async Task<List<Correction>> CorrectionsAsync(
        System.Data.Common.DbConnection connection,
        IReadOnlyCollection<string>? refs,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<Correction>(
                new CommandDefinition(
                    """
                    SELECT f.stream_id, f.intervention_id, f.ref, f.repo, f.person, f.person_mapped, f.period, f.went_wrong, f.went_wrong_label,
                           f.prevention, f.at, p.path
                    FROM casebox.steering_facts f
                    LEFT JOIN casebox.correction_paths p ON p.org_id = f.org_id AND p.stream_id = f.stream_id AND p.intervention_id = f.intervention_id
                    WHERE f.org_id = @Org AND f.intent = 'correction' AND f.went_wrong IS NOT NULL AND f.prevention IS NOT NULL
                      AND f.prevention <> 'nothing' AND f.at >= @Since AND (@Refs::text[] IS NULL OR f.ref = ANY(@Refs))
                    """,
                    new
                    {
                        Org,
                        Since = clock.GetUtcNow() - Window,
                        Refs = refs?.ToArray(),
                    },
                    cancellationToken: ct
                )
            )
        ).ToList();

    public static async Task<Dictionary<string, HashSet<string>>> WorkspacesAsync(
        System.Data.Common.DbConnection connection,
        string org,
        CancellationToken ct
    ) =>
        (
            await connection.QueryAsync<(string Name, string Repos)>(
                new CommandDefinition(
                    "SELECT name, repos::text FROM casebox.workspaces WHERE org_id = @Org",
                    new { Org = org },
                    cancellationToken: ct
                )
            )
        ).ToDictionary(
            w => w.Name,
            w => JsonSerializer.Deserialize<string[]>(w.Repos)!.ToHashSet(StringComparer.Ordinal)
        );

    private Task<Dictionary<string, HashSet<string>>> WorkspacesAsync(
        System.Data.Common.DbConnection connection,
        CancellationToken ct
    ) => WorkspacesAsync(connection, Org, ct);

    // The top-level path of each correction without one: the most common first segment of the
    // files its window names; "." for root files, "*" when it names none.
    private async Task PathsAsync(
        System.Data.Common.DbConnection connection,
        List<Correction> corrections,
        CancellationToken ct
    )
    {
        foreach (var stream in corrections.Where(c => c.Path is null).GroupBy(c => c.StreamId))
        {
            var found = await windows.ForAsync(
                stream.Key,
                [.. stream.Select(c => c.InterventionId)],
                ct
            );
            foreach (var w in found)
            {
                var files = w
                    .Before.Tools.SelectMany(t => t.Files ?? [])
                    .Concat(w.Files)
                    .Where(f => !string.IsNullOrWhiteSpace(f))
                    .ToList();
                var path =
                    files.Count == 0
                        ? "*"
                        : files
                            .Select(TopSegment)
                            .GroupBy(p => p)
                            .OrderByDescending(g => g.Count())
                            .ThenBy(g => g.Key, StringComparer.Ordinal)
                            .First()
                            .Key;
                await connection.ExecuteAsync(
                    new CommandDefinition(
                        """
                        INSERT INTO casebox.correction_paths (org_id, stream_id, intervention_id, path) VALUES (@Org, @Stream, @Id, @Path)
                        ON CONFLICT DO NOTHING
                        """,
                        new
                        {
                            Org,
                            Stream = stream.Key,
                            Id = w.InterventionId,
                            Path = path,
                        },
                        cancellationToken: ct
                    )
                );
            }
        }
    }

    public static string TopSegment(string file)
    {
        var f = file.Replace('\\', '/');
        while (f.StartsWith("./", StringComparison.Ordinal))
            f = f[2..];
        if (f.StartsWith('/') || f.StartsWith('~'))
            return "*";
        var slash = f.IndexOf('/');
        return slash < 0 ? "." : f[..slash];
    }

    private static string Hash(string text) =>
        Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(text)))[..20];
}
