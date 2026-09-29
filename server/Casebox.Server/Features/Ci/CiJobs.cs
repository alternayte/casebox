using System.Text.Json;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Jobs;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Ci;

public sealed record ResolveAnswer(IReadOnlyDictionary<string, string> Hashes);

// The worker's answer to harness.resolve: the harness hash each case gets from the baseline spec.
// The cases whose score is missing or stale become a baseline evaluation; the rest keep their score.
public sealed class ResolveResultHandler(TimeProvider clock) : IJobResultHandler
{
    public string Kind => CiEndpoints.ResolveJob;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var payload = result.Job.Payload;
        var ciRun = payload.GetProperty("ciRun").GetString()!;
        var answer =
            result.Result.Deserialize<ResolveAnswer>(EvaluationResults.Json)
            ?? throw new DomainException("The result is empty.");
        var connection = result.Transaction.Connection!;
        var run =
            await CiRuns.GetAsync(connection, result.Transaction, result.OrgId, ciRun, ct)
            ?? throw new NotFoundException($"CI run {ciRun} does not exist.");
        if (run.Status != CiRuns.Resolving)
            return;
        var request = JsonSerializer.Deserialize<CiEndpoints.BaselineRequest>(
            run.Request,
            EvaluationResults.Json
        )!;
        var repeats = request.Repeats ?? 3;
        var now = clock.GetUtcNow();

        var fresh = (
            await connection.QueryAsync<(string CaseId, string? HarnessHash)>(
                new CommandDefinition(
                    """
                    SELECT r.case_id, r.harness_hash
                    FROM casebox.run_results r
                    JOIN casebox.evaluations e ON e.org_id = r.org_id AND e.id = r.evaluation_id
                    WHERE r.org_id = @Org AND e.workspace = @Workspace AND e.purpose = 'baseline' AND e.spec_key = @Key
                      AND r.side = 'baseline' AND r.status = 'completed' AND r.created_at >= @Since
                    """,
                    new
                    {
                        Org = result.OrgId,
                        request.Workspace,
                        Key = HarnessSpec.Key(request.Spec),
                        Since = now - CiEndpoints.Freshness,
                    },
                    result.Transaction,
                    cancellationToken: ct
                )
            )
        ).GroupBy(r => (r.CaseId, r.HarnessHash)).Where(g => g.Count() >= repeats).Select(g => g.Key).ToHashSet();

        var missing = answer
            .Hashes.Where(h => !fresh.Contains((h.Key, h.Value)))
            .Select(h => h.Key)
            .Order(StringComparer.Ordinal)
            .ToList();
        if (missing.Count == 0)
        {
            await CiRuns.UpdateAsync(
                connection,
                result.Transaction,
                result.OrgId,
                ciRun,
                CiRuns.Skipped,
                null,
                $"The baseline is fresh: every case has {repeats} runs from the last {CiEndpoints.Freshness.Days} days with this harness, agent and model.",
                now,
                ct
            );
            return;
        }

        var planner = result.Services.GetRequiredService<Planner>();
        Plan plan;
        try
        {
            plan = await planner.PlanAsync(
                new EvaluationRequest(
                    request.Workspace,
                    request.Spec,
                    request.Spec,
                    "dev",
                    missing,
                    null,
                    repeats,
                    null,
                    request.CapUsd,
                    Purpose.Baseline,
                    request.Prices
                ),
                ciRun,
                null,
                ct
            );
            Planner.RequireMonthlyRoom(plan);
        }
        catch (DomainException ex) when (ex is not NotFoundException)
        {
            await CiRuns.UpdateAsync(
                connection,
                result.Transaction,
                result.OrgId,
                ciRun,
                CiRuns.Failed,
                null,
                ex.Message,
                now,
                ct
            );
            return;
        }

        var evaluationId = Ids.New();
        await result.Store.Execute<Evaluation>(
            Evaluation.StreamId(evaluationId),
            e => EvaluationDecider.Request(e, plan.Requested),
            ct
        );
        await CiRuns.UpdateAsync(
            connection,
            result.Transaction,
            result.OrgId,
            ciRun,
            CiRuns.Started,
            evaluationId,
            $"{missing.Count} of {answer.Hashes.Count} cases need a score.",
            now,
            ct
        );
    }
}
