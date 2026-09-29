using System.Text.Json;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Patterns;

public sealed record ClusterPart(string Label, string Summary, IReadOnlyList<string> Refs);

public sealed record ClusterAnswer(string? Model, IReadOnlyList<ClusterPart> Parts);

// The worker's split of one group (docs/specs/self-evolution.md, pattern.cluster). Each part that
// is ready by itself becomes a pattern, or adds its corrections to the open pattern of the same
// group that shares the most of them.
public sealed class ClusterResultHandler : IJobResultHandler
{
    public string Kind => PatternJobs.Cluster;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        result.Services.GetService<PatternScheduler>()?.Nudge();
        var payload = result.Job.Payload;
        var workspace = payload.GetProperty("workspace").GetString()!;
        var key = payload.GetProperty("key").Deserialize<PatternKey>(PatternJobs.Json)!;
        var asked = payload
            .GetProperty("corrections")
            .EnumerateArray()
            .Select(c => c.GetProperty("ref").GetString()!)
            .ToHashSet(StringComparer.Ordinal);
        var answer =
            result.Result.Deserialize<ClusterAnswer>(PatternJobs.Json)
            ?? throw new DomainException("The result is empty.");

        var scan = result.Services.GetRequiredService<PatternScan>();
        var connection = result.Transaction.Connection!;
        var facts = (await scan.CorrectionsAsync(connection, [.. asked], ct)).ToDictionary(c =>
            c.Ref
        );
        var (org, _) = await result.Store.Load<Organisation>(Organisation.StreamId, ct);
        var k = await result.Services.GetRequiredService<Solo>().ViewAsync(org, ct);

        var open = (
            await connection.QueryAsync<(string Id, string Refs)>(
                new CommandDefinition(
                    """
                    SELECT id, refs::text FROM casebox.patterns
                    WHERE org_id = @Org AND workspace = @Workspace AND went_wrong = @WentWrong AND label IS NOT DISTINCT FROM @Label
                      AND prevention = @Prevention AND path = @Path AND status IN ('open', 'acknowledged')
                    """,
                    new
                    {
                        Org = result.OrgId,
                        Workspace = workspace,
                        key.WentWrong,
                        key.Label,
                        key.Prevention,
                        key.Path,
                    },
                    result.Transaction,
                    cancellationToken: ct
                )
            )
        ).Select(p => (p.Id, Refs: JsonSerializer.Deserialize<string[]>(p.Refs)!.ToHashSet())).ToList();

        var used = new HashSet<string>(StringComparer.Ordinal);
        foreach (var part in answer.Parts ?? [])
        {
            // A ref belongs to one part at most, and only refs the job asked about count.
            var refs = (part.Refs ?? [])
                .Where(r => asked.Contains(r) && facts.ContainsKey(r) && used.Add(r))
                .ToList();
            if (!PatternScan.Ready(refs.Select(r => facts[r]), k))
                continue;
            var match = open.Select(p => (p.Id, Shared: p.Refs.Count(refs.Contains)))
                .Where(p => p.Shared > 0)
                .OrderByDescending(p => p.Shared)
                .Select(p => p.Id)
                .FirstOrDefault();
            if (match is not null)
            {
                await result.Store.Execute<Pattern>(
                    Pattern.StreamId(match),
                    p => PatternDecider.AddCorrections(p, refs),
                    ct
                );
                continue;
            }
            var id = Pattern.IdFor(result.OrgId, workspace, key, refs);
            await result.Store.Execute<Pattern>(
                Pattern.StreamId(id),
                p =>
                    PatternDecider.Detect(
                        p,
                        new PatternEvents.Detected(
                            workspace,
                            key.WentWrong,
                            key.Label,
                            key.Prevention,
                            key.Path,
                            Clip(part.Label, 80),
                            Clip(part.Summary, 400),
                            refs,
                            Pattern.IsAdvisory(key.Prevention)
                        )
                    ),
                ct
            );
            open.Add((id, refs.ToHashSet()));
        }
    }

    private static string Clip(string? text, int max)
    {
        var t = (text ?? "").Trim();
        return t.Length <= max ? t : t[..max];
    }
}
