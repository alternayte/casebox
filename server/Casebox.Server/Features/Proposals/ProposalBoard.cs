using System.Text.Json;
using Casebox.Server.Features.Evaluations;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Proposals;

// The proposal board: each proposal, its candidates with their scores, the gate's checks, the pull
// request and the post-merge outcome. Inline, so a request reads its own writes.
public sealed class ProposalBoard : Projection
{
    public const string Name = "proposal_board";

    public ProposalBoard()
    {
        On<ProposalEvents.Drafted>(
            async (e, ctx) =>
            {
                await Exec(
                    ctx,
                    """
                    INSERT INTO casebox.proposals (org_id, id, workspace, pattern, kind, status, repo, base_commit, ci_run, created_at, updated_at)
                    VALUES (@Org, @Id, @Workspace, @Pattern, @Kind, 'searching', @Repo, @BaseCommit, @CiRun, @At, @At) ON CONFLICT DO NOTHING
                    """,
                    new
                    {
                        e.Workspace,
                        e.Pattern,
                        Kind = SteeringFacts.Enum(e.Kind),
                        e.Repo,
                        e.BaseCommit,
                        e.CiRun,
                    }
                );
                foreach (var c in e.Candidates)
                    await InsertCandidate(ctx, c);
            }
        );
        On<ProposalEvents.CandidateMerged>((e, ctx) => InsertCandidate(ctx, e.Candidate));
        On<ProposalEvents.CandidateScored>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposal_candidates SET score = @Score::jsonb WHERE org_id = @Org AND proposal_id = @Id AND idx = @Index",
                    new { Score = JsonSerializer.Serialize(e, EvaluationResults.Json), e.Index }
                )
        );
        On<ProposalEvents.GateRequested>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'gating', gate_index = @Index, gate_evaluation = @Evaluation, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Index, e.Evaluation }
                )
        );
        On<ProposalEvents.GatePassed>((e, ctx) => Gate(ctx, "gate_passed", e.Checks, null));
        On<ProposalEvents.GateFailed>((e, ctx) => Gate(ctx, "gate_failed", e.Checks, null));
        On<ProposalEvents.GateInconclusive>(
            (e, ctx) => Gate(ctx, "gate_inconclusive", e.Checks, e.Reason)
        );
        On<ProposalEvents.PrOpened>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'pr_opened', pr_number = @Number, pr_url = @Url, branch = @Branch, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new
                    {
                        e.Number,
                        e.Url,
                        e.Branch,
                    }
                )
        );
        On<ProposalEvents.Merged>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'merged', merged_at = @MergedAt, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { MergedAt = e.At }
                )
        );
        On<ProposalEvents.Rejected>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET status = 'rejected', reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { e.Reason }
                )
        );
        On<ProposalEvents.OutcomeObserved>(
            (e, ctx) =>
                Exec(
                    ctx,
                    "UPDATE casebox.proposals SET outcome = @Outcome::jsonb, updated_at = @At WHERE org_id = @Org AND id = @Id",
                    new { Outcome = JsonSerializer.Serialize(e, EvaluationResults.Json) }
                )
        );
    }

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.proposal_candidates; DELETE FROM casebox.proposals;",
                transaction: context.Transaction,
                cancellationToken: context.CancellationToken
            )
        );

    private static Task InsertCandidate(ProjectionContext ctx, Candidate c) =>
        Exec(
            ctx,
            """
            INSERT INTO casebox.proposal_candidates (org_id, proposal_id, idx, edits, files, overrides, content_hash, rationale, merged_from)
            VALUES (@Org, @Id, @Index, @Edits::jsonb, @Files::jsonb, @Overrides, @ContentHash, @Rationale, @MergedFrom::jsonb) ON CONFLICT DO NOTHING
            """,
            new
            {
                c.Index,
                Edits = JsonSerializer.Serialize(c.Edits, EvaluationResults.Json),
                Files = JsonSerializer.Serialize(c.Edits.Select(x => x.File).Distinct()),
                c.Overrides,
                c.ContentHash,
                c.Rationale,
                MergedFrom = c.MergedFrom is null ? null : JsonSerializer.Serialize(c.MergedFrom),
            }
        );

    private static Task Gate(
        ProjectionContext ctx,
        string status,
        IReadOnlyList<GateCheck> checks,
        string? reason
    ) =>
        Exec(
            ctx,
            "UPDATE casebox.proposals SET status = @Status, checks = @Checks::jsonb, reason = @Reason, updated_at = @At WHERE org_id = @Org AND id = @Id",
            new
            {
                Status = status,
                Checks = JsonSerializer.Serialize(checks, EvaluationResults.Json),
                Reason = reason,
            }
        );

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Id", ctx.StreamId["proposal:".Length..]);
        parameters.Add("At", ctx.OccurredAt);
        return ctx.Connection.ExecuteAsync(
            new CommandDefinition(
                sql,
                parameters,
                ctx.Transaction,
                cancellationToken: ctx.CancellationToken
            )
        );
    }
}
