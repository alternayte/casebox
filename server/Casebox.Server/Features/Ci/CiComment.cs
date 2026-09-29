using System.Globalization;
using System.Text;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Integrations;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Ci;

// Posts or updates the harness CI comment of a pull request when its evaluation reaches a verdict.
// The comment is found by its hidden marker, so a second delivery of the same message updates the
// one comment instead of adding another (SDD section 10, Outbox).
public sealed class CiCommentEffect(
    NpgsqlDataSource db,
    IEventStore store,
    DeedboxContext context,
    GitHubClients github
) : IEffectHandler
{
    public const string Topic = "effect.ci_comment";

    public string Kind => "ci-comment";

    public async Task HandleAsync(EffectMessage message, CancellationToken ct)
    {
        // Deedbox writes the tenant into the outbox row's headers, and QueueBox forwards them.
        var org =
            message.Headers.GetValueOrDefault("x-deedbox-tenant-id")
            ?? throw new InvalidOperationException("The message names no organisation.");
        var evaluationId = message.Payload.GetProperty("evaluationId").GetString()!;
        context.TenantId = org;

        await using var connection = await db.OpenConnectionAsync(ct);
        var run = await CiRuns.ForEvaluationAsync(connection, org, evaluationId, ct);
        if (run is not { Kind: CiRuns.PullRequest, Number: { } number })
            return;
        var view = await CiEndpoints.ViewAsync(connection, org, run, store, ct);
        var client =
            await github.ForOrgAsync(org, ct)
            ?? throw new InvalidOperationException(
                $"GitHub is not connected, so the harness CI comment cannot be posted ({Cbx.GitHubNotConnected})."
            );

        var marker = Marker(run.Workspace);
        var body = Render(view, run, marker);
        var path = CiRuns.ApiPath(run.Repo);
        var comments = await client.ListAsync(
            $"repos/{path}/issues/{number}/comments",
            null,
            20,
            ct
        );
        var existing = comments.FirstOrDefault(c =>
            c.TryGetProperty("body", out var b) && b.GetString()?.Contains(marker) == true
        );
        if (existing.ValueKind == System.Text.Json.JsonValueKind.Object)
            await client.SendJsonAsync(
                HttpMethod.Patch,
                $"repos/{path}/issues/comments/{existing.GetProperty("id").GetInt64()}",
                new { body },
                ct
            );
        else
            await client.SendJsonAsync(
                HttpMethod.Post,
                $"repos/{path}/issues/{number}/comments",
                new { body },
                ct
            );
    }

    // One comment per pull request and workspace; a shared harness has one per workspace.
    public static string Marker(string? workspace) =>
        $"<!-- casebox:harness-ci:{workspace ?? ""} -->";

    public static string Render(CiEndpoints.RunView v, CiRun run, string marker)
    {
        var s = new StringBuilder();
        s.AppendLine(marker);
        s.AppendLine(
            Inv($"### Casebox harness CI: workspace `{v.Workspace}` at `{Short(run.HeadSha)}`")
        );
        s.AppendLine();
        var verdict = v.Verdict;
        var regressions = v.Cases.Count(c => c.Regression);
        if (verdict is null)
            s.AppendLine(Inv($"No verdict: {v.Message ?? v.Status}."));
        else
        {
            s.AppendLine(
                regressions == 0
                    ? "**No regression found.** No case the baseline passed in every run failed with this change."
                    : Inv(
                        $"**{regressions} regression{(regressions == 1 ? "" : "s")}.** The baseline passed {(regressions == 1 ? "this case" : "these cases")} in every run, and the candidate failed in every run."
                    )
            );
            s.AppendLine();
            s.AppendLine(Inv($"- Verdict: **{Word(verdict)}**. {Rule(verdict, v.Delta)}"));
            s.AppendLine(
                Inv(
                    $"- Δ pass rate (candidate − baseline): {Pts(verdict.Delta)}, {verdict.Level * 100:0.#}% interval {Pts(verdict.Lower)} to {Pts(verdict.Upper)}, over {verdict.Cases} cases and {verdict.Runs} candidate runs."
                )
            );
            if (v.Estimate?.DetectableEffect is { } effect)
                s.AppendLine(
                    Inv(
                        $"- At this size the comparison detects a difference of about {effect * 100:0} points or more (power 0.8). A smoke run does not claim \"better\"."
                    )
                );
        }
        s.AppendLine(
            Inv(
                $"- Cost: {v.SpentUsd:0.00} USD of an estimated {v.Estimate?.TotalUsd ?? 0:0.00} USD; {v.RunsFailed} runs failed to run."
            )
        );
        if (v.Baseline is { } b)
            s.AppendLine(
                Inv(
                    $"- Baseline: the default branch's harness `{Short(b.HarnessHash)}`, scored {b.ScoredAt:yyyy-MM-dd} by the nightly run, not run again here."
                )
            );
        s.AppendLine();
        s.AppendLine("| Case | Baseline passed | Candidate passed | |");
        s.AppendLine("| --- | --- | --- | --- |");
        foreach (var c in v.Cases)
            s.AppendLine(
                Inv(
                    $"| `{c.CaseId}` | {c.BaselinePassed}/{c.BaselineRuns} | {c.CandidatePassed}/{c.CandidateRuns} | {(c.Regression ? "regression" : c.FailedRuns > 0 ? $"{c.FailedRuns} failed to run" : "")} |"
                )
            );
        s.AppendLine();
        s.AppendLine(
            Inv(
                $"A smoke run compares the candidate with a cached baseline. For a verdict with its full interval, run `casebox compare --candidate harness={run.HeadSha}` in a checkout of this pull request."
            )
        );
        if (!string.IsNullOrWhiteSpace(run.ServerUrl) && v.EvaluationId is { } id)
            s.AppendLine(Inv($"Details: {run.ServerUrl.TrimEnd('/')}/evaluations/{id}"));
        return s.ToString();
    }

    private static string Word(EvaluationEvents.VerdictReached v) =>
        v.Reason == EvaluationSteps.SmokeReason ? "inconclusive (smoke run)"
        : v.Reason == Statistics.NoDifference ? "inconclusive, no difference detected"
        : Steering.SteeringFacts.Enum(v.Verdict);

    private static string Rule(EvaluationEvents.VerdictReached v, double delta)
    {
        var level = Inv($"{v.Level * 100:0.#}%");
        return v.Verdict switch
        {
            Statistics.Verdict.Worse => $"The {level} interval lies entirely below 0.",
            _ when v.Reason == EvaluationSteps.SmokeReason =>
                "The interval does not include a regression; a smoke run never claims better or equivalent.",
            _ when v.Reason == "budget" => "The budget ran out before the run finished.",
            _ when v.Cases < EvaluationDecider.MinimumCases => Inv(
                $"Only {v.Cases} cases completed; a verdict needs at least {EvaluationDecider.MinimumCases}."
            ),
            _ => Inv(
                $"The {level} interval neither lies on one side of 0 nor within ±{delta * 100:0.#} points."
            ),
        };
    }

    private static string Pts(double share) => Inv($"{share * 100:+0.0;-0.0;0.0} points");

    private static string Short(string? sha) => sha is { Length: > 12 } ? sha[..12] : sha ?? "";

    private static string Inv(FormattableString s) => s.ToString(CultureInfo.InvariantCulture);
}
