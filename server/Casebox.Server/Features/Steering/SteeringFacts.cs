using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Dapper;
using Deedbox;

namespace Casebox.Server.Features.Steering;

// The steering-facts projection: one row per intervention with its current labels and where they
// came from. Inline, so a refresh reads its own writes. It scrubs on SubjectErased: the erased
// person's rows go, so a report after an erasure equals a rebuild.
public sealed class SteeringFacts : Projection
{
    public const string Name = "steering_facts";

    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web) { Converters = { Infrastructure.CaseboxStreams.Enums } };

    public SteeringFacts()
    {
        On<SteeringEvents.Observed>((e, ctx) => Exec(ctx,
            """
            INSERT INTO casebox.steering_facts (org_id, stream_id, intervention_id, ref, signal, phase, at, repo, session_id, number, person, person_mapped, period,
                text, refs, rule_intent, status, intent, label_source)
            VALUES (@Org, @Stream, @Id, @Ref, @Signal, @Phase, @EventAt, @Repo, @SessionId, @Number, @Person, @PersonMapped, @Period,
                @Text, @Refs::jsonb, @RuleIntent, 'pending', @RuleIntent, CASE WHEN @RuleIntent IS NULL THEN NULL ELSE 'rule' END)
            ON CONFLICT DO NOTHING
            """,
            new
            {
                Id = e.InterventionId, Ref = RefOf(ctx.TenantId, ctx.StreamId, e.InterventionId), Signal = Enum(e.Signal), Phase = Enum(e.Phase), EventAt = e.At, e.Repo,
                e.SessionId, e.Number, e.Person, e.PersonMapped, e.Period, e.Text, Refs = JsonSerializer.Serialize(e.Refs, Json),
                RuleIntent = e.RuleIntent is { } r ? Enum(r) : null,
            }));

        // The model's labels are kept apart, so agreement with the team's relabels stays measurable.
        On<SteeringEvents.Classified>((e, ctx) => Exec(ctx,
            """
            UPDATE casebox.steering_facts SET
                model = @Model, model_intent = @Intent, model_went_wrong = @WentWrong, model_prevention = @Prevention, confidence = @Confidence,
                status = 'classified', unclassified_reason = NULL,
                intent = CASE WHEN label_source = 'human' THEN intent ELSE @Intent END,
                went_wrong = CASE WHEN label_source = 'human' THEN went_wrong ELSE @WentWrong END,
                went_wrong_label = CASE WHEN label_source = 'human' THEN went_wrong_label ELSE @Label END,
                prevention = CASE WHEN label_source = 'human' THEN prevention ELSE @Prevention END,
                label_source = CASE WHEN label_source = 'human' THEN 'human' WHEN rule_intent IS NOT NULL THEN 'rule' ELSE 'model' END
            WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = @Id
            """,
            new
            {
                Id = e.InterventionId, e.Model, Intent = Enum(e.Intent), WentWrong = e.WentWrong is { } w ? Enum(w) : null, Label = e.WentWrongLabel,
                Prevention = e.Prevention is { } p ? Enum(p) : null, e.Confidence,
            }));

        // A failed classification never replaces labels a model or a person already gave. A rule
        // correction stays a correction without its what-went-wrong.
        On<SteeringEvents.Unclassified>((e, ctx) => Exec(ctx,
            """
            UPDATE casebox.steering_facts SET status = 'unclassified', unclassified_reason = @Reason, confidence = @Confidence, model = coalesce(@Model, model)
            WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = @Id AND status = 'pending'
            """,
            new { Id = e.InterventionId, Reason = Enum(e.Reason), e.Confidence, e.Model }));

        On<SteeringEvents.Relabeled>((e, ctx) => Exec(ctx,
            """
            UPDATE casebox.steering_facts SET
                intent = @Intent, went_wrong = @WentWrong, went_wrong_label = @Label, prevention = @Prevention,
                label_source = 'human', status = 'classified', unclassified_reason = NULL, relabeled_at = @At
            WHERE org_id = @Org AND stream_id = @Stream AND intervention_id = @Id
            """,
            new
            {
                Id = e.InterventionId, Intent = Enum(e.Intent), WentWrong = e.WentWrong is { } w ? Enum(w) : null, Label = e.WentWrongLabel,
                Prevention = e.Prevention is { } p ? Enum(p) : null,
            }));

        On<SubjectErased>((e, ctx) => Exec(ctx,
            "DELETE FROM casebox.steering_facts WHERE org_id = @Org AND stream_id = @Stream AND person = @Subject",
            new { Subject = e.SubjectId }));
    }

    // An opaque reference for the API: it names no session, pull request or person.
    public static string RefOf(string org, string stream, string interventionId) =>
        Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes($"{org}|{stream}|{interventionId}")))[..24];

    public static string Enum<T>(T value) where T : struct, System.Enum => JsonSerializer.Serialize(value, Json).Trim('"');

    public static T? Parse<T>(string? value) where T : struct, System.Enum =>
        value is null ? null : JsonSerializer.Deserialize<T>($"\"{value}\"", Json);

    protected override Task ResetAsync(WriteContext context) =>
        context.Connection.ExecuteAsync(new CommandDefinition("DELETE FROM casebox.steering_facts", transaction: context.Transaction, cancellationToken: context.CancellationToken));

    private static Task Exec(ProjectionContext ctx, string sql, object values)
    {
        var parameters = new DynamicParameters(values);
        parameters.Add("Org", ctx.TenantId);
        parameters.Add("Stream", ctx.StreamId);
        parameters.Add("At", ctx.OccurredAt);
        return ctx.Connection.ExecuteAsync(new CommandDefinition(sql, parameters, ctx.Transaction, cancellationToken: ctx.CancellationToken));
    }
}
