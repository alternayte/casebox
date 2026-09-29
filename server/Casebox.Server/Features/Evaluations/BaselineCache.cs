using System.Data.Common;
using Dapper;

namespace Casebox.Server.Features.Evaluations;

// The cached baseline of harness CI and the proposer's search: each approved dev case's runs in
// the latest baseline evaluation that scored it under a spec key (docs/specs/harness-ci.md).
public static class BaselineCache
{
    public static async Task<Dictionary<string, BaselineScore>> ScoresAsync(
        DbConnection connection,
        string org,
        string workspace,
        string specKey,
        string? sealedRepo,
        CancellationToken ct
    )
    {
        var rows = await connection.QueryAsync<(
            string CaseId,
            string EvaluationId,
            DateTime DoneAt,
            bool Passed,
            decimal CostUsd,
            double? Seconds,
            string? HarnessHash
        )>(
            new CommandDefinition(
                """
                WITH latest AS (
                    SELECT DISTINCT ON (r.case_id) r.case_id, e.id AS evaluation_id, e.done_at
                    FROM casebox.run_results r
                    JOIN casebox.evaluations e ON e.org_id = r.org_id AND e.id = r.evaluation_id
                    WHERE r.org_id = @Org AND e.workspace = @Workspace AND e.purpose = 'baseline' AND e.status = 'done'
                      AND e.spec_key = @Key AND r.status = 'completed' AND r.side = 'baseline'
                    ORDER BY r.case_id, e.done_at DESC
                )
                SELECT l.case_id, l.evaluation_id, l.done_at, r.passed, r.cost_usd, r.seconds, r.harness_hash
                FROM latest l
                JOIN casebox.run_results r ON r.org_id = @Org AND r.evaluation_id = l.evaluation_id AND r.case_id = l.case_id
                    AND r.status = 'completed' AND r.side = 'baseline'
                JOIN casebox.case_catalog c ON c.org_id = @Org AND c.id = l.case_id
                WHERE c.status = 'approved' AND c.split = 'dev' AND c.instruction IS NOT NULL
                  AND (@Sealed::text IS NULL OR EXISTS (
                      SELECT 1 FROM jsonb_array_elements(c.repos) x WHERE x->>'repo' = @Sealed AND x->>'role' = 'sealed'))
                ORDER BY l.case_id, r.run_id
                """,
                new
                {
                    Org = org,
                    Workspace = workspace,
                    Key = specKey,
                    Sealed = sealedRepo,
                },
                cancellationToken: ct
            )
        );
        return rows.GroupBy(r => r.CaseId)
            .ToDictionary(
                g => g.Key,
                g =>
                {
                    var runs = g.ToList();
                    return new BaselineScore(
                        [.. runs.Select(r => r.Passed)],
                        [.. runs.Select(r => r.CostUsd)],
                        [.. runs.Select(r => r.Seconds ?? 0)],
                        runs.Select(r => r.HarnessHash).FirstOrDefault(h => h is not null),
                        runs[0].EvaluationId,
                        new DateTimeOffset(DateTime.SpecifyKind(runs[0].DoneAt, DateTimeKind.Utc))
                    );
                }
            );
    }
}
