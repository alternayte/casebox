using System.Globalization;
using System.Net;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Privacy;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Proposals;

// Opens a proposal's pull request when its gate passed (docs/specs/self-evolution.md, The pull
// request). Every step finds what an earlier delivery made: the branch is named after the proposal,
// the pull request is found by its head, and the decider refuses a second pr_opened.
public sealed class ProposalPrEffect(
    NpgsqlDataSource db,
    IEventStore store,
    DeedboxContext context,
    GitHubClients github,
    ProposalSteps steps
) : IEffectHandler
{
    public const string Topic = "effect.proposal_pr";

    public string Kind => "proposal-pr";

    public static string Branch(string proposal) =>
        $"casebox/proposal-{proposal.ToLowerInvariant()}";

    public async Task HandleAsync(EffectMessage message, CancellationToken ct)
    {
        var org =
            message.Headers.GetValueOrDefault("x-deedbox-tenant-id")
            ?? throw new InvalidOperationException("The message names no organisation.");
        var id = message.Payload.GetProperty("proposalId").GetString()!;
        context.TenantId = org;
        context.Metadata = new EventMetadata { Actor = "system:proposer" };

        var (p, _) = await store.Load<Proposal>(Proposal.StreamId(id), ct);
        if (!p.Exists || p.Pr is not null || p.Status != ProposalStatus.GatePassed)
            return;
        var draft = p.Draft!;
        var candidate = p.Gated!;
        var client =
            await github.ForOrgAsync(org, ct)
            ?? throw new InvalidOperationException(
                $"GitHub is not connected, so the proposal\'s pull request cannot open ({Cbx.GitHubNotConnected})."
            );
        var path = Ci.CiRuns.ApiPath(draft.Repo);
        var branch = Branch(id);
        var overrides = await steps.FilesAsync(candidate.Overrides, ct);

        // The branch: one commit on the base commit with the candidate's files.
        if (!await ExistsAsync(client, $"repos/{path}/git/ref/heads/{branch}", ct))
        {
            var baseCommit = await client.GetAsync(
                $"repos/{path}/git/commits/{draft.BaseCommit}",
                ct
            );
            var tree = await client.SendJsonAsync(
                HttpMethod.Post,
                $"repos/{path}/git/trees",
                new
                {
                    base_tree = baseCommit.GetProperty("tree").GetProperty("sha").GetString(),
                    tree = overrides.Files.Select(f => new Dictionary<string, object?>
                    {
                        ["path"] = f.Key,
                        ["mode"] = "100644",
                        ["type"] = "blob",
                        [f.Value is null ? "sha" : "content"] = f.Value,
                    }),
                },
                ct
            );
            var commit = await client.SendJsonAsync(
                HttpMethod.Post,
                $"repos/{path}/git/commits",
                new
                {
                    message = $"Casebox proposal {id}\n\n{candidate.Rationale}",
                    tree = tree.GetProperty("sha").GetString(),
                    parents = new[] { draft.BaseCommit },
                },
                ct
            );
            await client.SendJsonAsync(
                HttpMethod.Post,
                $"repos/{path}/git/refs",
                new { @ref = $"refs/heads/{branch}", sha = commit.GetProperty("sha").GetString() },
                ct
            );
        }

        // The pull request: found by its head, or opened.
        var owner = path.Split('/')[0];
        var existing = await client.ListAsync(
            $"repos/{path}/pulls?state=all&head={owner}:{branch}",
            null,
            1,
            ct
        );
        var pull = existing.FirstOrDefault();
        if (pull.ValueKind != JsonValueKind.Object)
        {
            var repo = await client.GetAsync($"repos/{path}", ct);
            var (org_, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
            await using var connection = await db.OpenConnectionAsync(ct);
            var evidence = draft.Pattern is null
                ? null
                : await PatternEndpoints.EvidenceAsync(
                    connection,
                    org,
                    draft.Pattern,
                    KView.Of(org_.Settings.K),
                    ct
                );
            var gate = await GateAsync(connection, org, p.GateEvaluation!, ct);
            pull = await client.SendJsonAsync(
                HttpMethod.Post,
                $"repos/{path}/pulls",
                new
                {
                    title = draft.Kind == ProposalKind.Removal
                        ? $"Casebox: remove {string.Join(", ", candidate.Edits.Select(e => e.Heading ?? e.File))}"
                        : $"Casebox: {evidence?.Title ?? "harness edit"}",
                    head = branch,
                    @base = repo.GetProperty("default_branch").GetString(),
                    body = Body(id, draft, candidate, evidence, gate, p),
                },
                ct
            );
        }

        await store.Execute<Proposal>(
            Proposal.StreamId(id),
            s =>
                ProposalDecider.OpenPr(
                    s,
                    new ProposalEvents.PrOpened(
                        draft.Repo,
                        pull.GetProperty("number").GetInt32(),
                        branch,
                        pull.GetProperty("html_url").GetString() ?? ""
                    )
                ),
            ct
        );
    }

    private static async Task<bool> ExistsAsync(
        GitHubClient client,
        string path,
        CancellationToken ct
    )
    {
        try
        {
            await client.GetAsync(path, ct);
            return true;
        }
        catch (HttpRequestException e) when (e.StatusCode == HttpStatusCode.NotFound)
        {
            return false;
        }
    }

    private static async Task<(EvaluationEvents.VerdictReached? Verdict, decimal Spent)> GateAsync(
        System.Data.Common.DbConnection connection,
        string org,
        string evaluation,
        CancellationToken ct
    )
    {
        var row = await Dapper.SqlMapper.QuerySingleOrDefaultAsync<(
            string? Verdict,
            decimal Spent
        )>(
            connection,
            new Dapper.CommandDefinition(
                "SELECT verdict::text, spent_usd FROM casebox.evaluations WHERE org_id = @Org AND id = @Id",
                new { Org = org, Id = evaluation },
                cancellationToken: ct
            )
        );
        return (
            row.Verdict is null
                ? null
                : JsonSerializer.Deserialize<EvaluationEvents.VerdictReached>(
                    row.Verdict,
                    EvaluationResults.Json
                ),
            row.Spent
        );
    }

    // The body: the pattern, the change, the evidence (with at least k people behind it), the gate
    // report, and how to undo it. Never a person token.
    public static string Body(
        string id,
        ProposalEvents.Drafted draft,
        Candidate candidate,
        PatternEndpoints.Evidence? evidence,
        (EvaluationEvents.VerdictReached? Verdict, decimal Spent) gate,
        Proposal p
    )
    {
        string Inv(FormattableString f) => f.ToString(CultureInfo.InvariantCulture);
        var s = new StringBuilder();
        s.AppendLine(
            draft.Kind == ProposalKind.Removal
                ? "Casebox's harness diet found that removing this part of the harness keeps quality and lowers cost."
                : Inv(
                    $"This change targets a recurring correction: {evidence?.Summary ?? "see the pattern"} (model-generated summary)."
                )
        );
        s.AppendLine();
        s.AppendLine("### The change");
        foreach (var e in candidate.Edits)
            s.AppendLine(
                Inv($"- `{e.Op}` in `{e.File}`{(e.Heading is null ? "" : $" under “{e.Heading}”")}")
            );
        s.AppendLine();
        s.AppendLine(Inv($"Why: {candidate.Rationale}"));
        if (evidence is not null)
        {
            s.AppendLine();
            s.AppendLine("### Evidence");
            if (evidence.MeetsK)
            {
                s.AppendLine(
                    Inv(
                        $"- {evidence.Corrections} corrections from {evidence.People} people (observational)."
                    )
                );
                foreach (var q in evidence.Quotes)
                    s.AppendLine(Inv($"- > {q.Text.ReplaceLineEndings(" ")} ({q.Day})"));
            }
            else
                s.AppendLine(
                    "- Fewer people than the organisation's k are behind this pattern now, so counts and quotes are hidden."
                );
            if (evidence.Cases.Count > 0)
                s.AppendLine(
                    Inv($"- Cases: {string.Join(", ", evidence.Cases.Select(c => $"`{c.Id}`"))}")
                );
        }
        s.AppendLine();
        s.AppendLine("### Gate report (held-out cases, controlled comparison)");
        if (gate.Verdict is { } v)
        {
            s.AppendLine(
                Inv(
                    $"- Verdict: **{v.Verdict.ToString().ToLowerInvariant()}**{(v.EquivalentAndCheaper ? " and cheaper" : "")}. Δ pass rate {v.Delta * 100:+0.0;-0.0} points, {v.Level * 100:0.#}% interval {v.Lower * 100:+0.0;-0.0} to {v.Upper * 100:+0.0;-0.0}, {v.Cases} cases, {v.Runs} runs."
                )
            );
            if (v.CostRatio is { } r)
                s.AppendLine(
                    Inv(
                        $"- Cost per task, candidate ÷ baseline: {r:0.00} (interval {v.CostLower:0.00} to {v.CostUpper:0.00})."
                    )
                );
        }
        s.AppendLine(Inv($"- Gate cost: {gate.Spent:0.00} USD."));
        s.AppendLine();
        s.AppendLine("| Check | Passed | Evidence | Detail |");
        s.AppendLine("| --- | --- | --- | --- |");
        foreach (var c in GateChecks(p))
            s.AppendLine(
                Inv($"| {c.Name} | {(c.Passed ? "yes" : "no")} | {c.Evidence} | {c.Detail} |")
            );
        s.AppendLine();
        s.AppendLine("Undo: revert this pull request.");
        s.AppendLine();
        s.AppendLine(Inv($"<!-- casebox:proposal:{id} -->"));
        return s.ToString();
    }

    private static IReadOnlyList<GateCheck> GateChecks(Proposal p) => p.GateChecks ?? [];
}
