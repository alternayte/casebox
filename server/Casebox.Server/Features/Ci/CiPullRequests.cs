using System.Security.Claims;
using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Workspaces;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Ci;

// A pull request's harness CI: the smoke suite from cached baseline scores, one candidate-only
// evaluation per workspace, and the older push's evaluation cancelled (docs/specs/harness-ci.md).
public sealed class CiPullRequests(
    NpgsqlDataSource db,
    IEventStore store,
    Planner planner,
    DeedboxContext context,
    TimeProvider clock
)
{
    private string Org => context.TenantId;

    public async Task<CiEndpoints.Started> StartAsync(
        CiEndpoints.PullRequestRequest body,
        ClaimsPrincipal user,
        CancellationToken ct
    )
    {
        if (body.Spec is null || string.IsNullOrWhiteSpace(body.Repo) || body.Number < 1)
            throw new DomainException("A pull request names its repository, number and spec.");
        if (string.IsNullOrWhiteSpace(body.HeadSha) || body.HeadSha.StartsWith('-'))
            throw new DomainException("A pull request names its head commit.");
        if (body.Size is < 1 or > 100)
            throw new DomainException("A smoke suite has 1 to 100 cases.");
        if (body.Repeats is < 1 or > 10)
            throw new DomainException("A smoke suite runs 1 to 10 repeats.");
        var repo = WorkspaceDecider.NormalizeRepo(body.Repo);
        var baseline = CiEndpoints.Normalize(body.Spec);
        Planner.Check(baseline, "baseline");

        // The candidate changes the harness only: the pull request's repository at its head, or
        // the shared harness at its head.
        HarnessSpec candidate;
        List<string> workspaces;
        if (body.OnSharedHarness)
        {
            if (baseline.Shared?.Repo != repo)
                throw new DomainException(
                    $"The baseline's shared harness is not {repo}, the repository of this pull request."
                );
            candidate = baseline with { Shared = baseline.Shared with { Ref = body.HeadSha } };
            await using var c = await db.OpenConnectionAsync(ct);
            workspaces = (
                await c.QueryAsync<string>(
                    new CommandDefinition(
                        "SELECT name FROM casebox.workspaces WHERE org_id = @Org AND shared_harness = @Repo ORDER BY name",
                        new { Org, Repo = repo },
                        cancellationToken: ct
                    )
                )
            ).ToList();
            if (workspaces.Count == 0)
                throw new DomainException(
                    $"No workspace records {repo} as its shared harness. casebox ci --baseline in a workspace repository records harness.shared of its casebox.yml."
                );
        }
        else
        {
            if (string.IsNullOrWhiteSpace(body.Workspace))
                throw new DomainException(
                    "A pull request outside a shared harness names its workspace."
                );
            await CiEndpoints.ConfigureAsync(store, body.Workspace, body.Globs, body.Shared, ct);
            candidate = baseline with { Harness = $"{repo}@{body.HeadSha}" };
            workspaces = [body.Workspace];
        }

        await CancelOlderAsync(repo, body.Number, user, ct);

        var size = body.Size ?? 10;
        var shares = Enumerable
            .Range(0, workspaces.Count)
            .Select(i => size / workspaces.Count + (i < size % workspaces.Count ? 1 : 0))
            .ToList();
        var runs = new List<CiEndpoints.RunRef>();
        for (var i = 0; i < workspaces.Count; i++)
        {
            if (shares[i] == 0)
                continue;
            runs.Add(
                await StartOneAsync(
                    body,
                    repo,
                    workspaces[i],
                    shares[i],
                    body.OnSharedHarness ? null : repo,
                    baseline,
                    candidate,
                    user,
                    ct
                )
            );
        }
        return new CiEndpoints.Started(runs);
    }

    private async Task<CiEndpoints.RunRef> StartOneAsync(
        CiEndpoints.PullRequestRequest body,
        string repo,
        string workspace,
        int size,
        string? sealedRepo,
        HarnessSpec baseline,
        HarnessSpec candidate,
        ClaimsPrincipal user,
        CancellationToken ct
    )
    {
        var id = Ids.New();
        var now = clock.GetUtcNow();
        var scores = await SelectAsync(
            workspace,
            sealedRepo,
            HarnessSpec.Key(baseline),
            repo,
            body.Number,
            size,
            ct
        );

        string status;
        string? message = null;
        string? evaluationId = null;
        Plan? plan = null;
        if (scores.Count == 0)
        {
            status = CiRuns.Skipped;
            message =
                $"No approved dev case of workspace {workspace} has a baseline score for this agent and model yet. Run casebox ci --baseline on the default branch (the scheduled Action does it nightly), then push again.";
        }
        else
        {
            try
            {
                plan = await planner.PlanAsync(
                    new EvaluationRequest(
                        workspace,
                        baseline,
                        candidate,
                        "dev",
                        [.. scores.Keys],
                        null,
                        body.Repeats ?? 1,
                        null,
                        body.CapUsd,
                        Purpose.HarnessCi,
                        body.Prices
                    ),
                    id,
                    scores,
                    ct
                );
                Planner.RequireMonthlyRoom(plan);
                status = CiRuns.Started;
                evaluationId = Ids.New();
            }
            catch (DomainException ex) when (ex is not NotFoundException)
            {
                status = CiRuns.Failed;
                message = ex.Message;
            }
        }

        await using var connection = await db.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        await CiRuns.InsertAsync(
            connection,
            transaction,
            Org,
            new CiRun(
                id,
                CiRuns.PullRequest,
                workspace,
                repo,
                body.Number,
                body.HeadSha,
                body.BaseSha,
                body.ServerUrl,
                evaluationId,
                status,
                message,
                JsonSerializer.Serialize(body, EvaluationResults.Json),
                user.Actor()!,
                now.UtcDateTime
            ),
            ct
        );
        if (plan is not null)
            await store
                .UseTransaction(transaction)
                .Execute<Evaluation>(
                    Evaluation.StreamId(evaluationId!),
                    e => EvaluationDecider.Request(e, plan.Requested),
                    ct
                );
        await transaction.CommitAsync(ct);
        return new CiEndpoints.RunRef(id, workspace, status, evaluationId, message);
    }

    // Only the latest push runs: a new request cancels the open evaluations of the same pull request.
    private async Task CancelOlderAsync(
        string repo,
        int number,
        ClaimsPrincipal user,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var open = await connection.QueryAsync<string>(
            new CommandDefinition(
                """
                SELECT c.evaluation_id FROM casebox.ci_runs c
                JOIN casebox.evaluations e ON e.org_id = c.org_id AND e.id = c.evaluation_id
                WHERE c.org_id = @Org AND c.kind = 'pull_request' AND c.repo = @Repo AND c.number = @Number
                  AND e.status IN ('running', 'awaiting_confirmation')
                """,
                new
                {
                    Org,
                    Repo = repo,
                    Number = number,
                },
                cancellationToken: ct
            )
        );
        foreach (var evaluation in open)
            await store.Execute<Evaluation>(
                Evaluation.StreamId(evaluation),
                e =>
                    EvaluationDecider.Cancel(
                        e,
                        user.Actor() ?? "ci",
                        "a newer push of the pull request"
                    ),
                ct
            );
    }

    // The smoke suite: approved dev cases with a baseline score under this spec key (in the pull
    // request's repository when it is sealed there), the ones the baseline passed in every run
    // first, then in a stable order per pull request.
    private async Task<Dictionary<string, BaselineScore>> SelectAsync(
        string workspace,
        string? sealedRepo,
        string specKey,
        string repo,
        int number,
        int size,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var scores = await BaselineCache.ScoresAsync(
            connection,
            Org,
            workspace,
            specKey,
            sealedRepo,
            ct
        );
        return scores
            .OrderBy(c => c.Value.Passed.All(p => p) ? 0 : 1)
            .ThenBy(c => CiRuns.Order(repo, number, c.Key), StringComparer.Ordinal)
            .Take(size)
            .ToDictionary(c => c.Key, c => c.Value);
    }
}
