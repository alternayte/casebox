using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Privacy;

public sealed record ErasureResult(int Subjects, int Sessions);

// Erases a person everywhere: every token the identity and its roster aliases had in every period
// that still has a secret. Deedbox deletes the subject keys, so their correction text becomes
// unreadable in every stream at once; Casebox deletes their sessions, trace events and telemetry.
public sealed class Erasure(IPseudonyms pseudonyms, IEventStoreAdmin admin, Roster roster, NpgsqlDataSource db, IEventStore store, DeedboxContext context)
{
    public async Task<ErasureResult> EraseAsync(string rawIdentity, CancellationToken ct)
    {
        var colon = rawIdentity.IndexOf(':', StringComparison.Ordinal);
        var identity = colon > 0 ? Identities.Identity(rawIdentity[..colon].Trim().ToLowerInvariant(), rawIdentity[(colon + 1)..]) : null;
        if (identity is null) throw new DomainException("Name the identity with its kind, such as email:alice@example.com, github:alice or jira:alice.");

        var orgId = context.TenantId;
        var identities = await roster.AliasesOfAsync(orgId, identity, ct);

        await using var connection = await db.OpenConnectionAsync(ct);
        var periods = (await connection.QueryAsync<string>(new CommandDefinition(
            "SELECT DISTINCT period FROM casebox.sessions WHERE org_id = @Org", new { Org = orgId }, cancellationToken: ct))).ToList();

        var subjects = new HashSet<string>(StringComparer.Ordinal);
        foreach (var period in periods)
        foreach (var id in identities)
        {
            try
            {
                subjects.Add(await pseudonyms.SubjectForAsync(id, period, ct));
            }
            catch (DeedboxException e) when (e.Code == "DBX036")
            {
                // The period's secret is destroyed: nobody can link its tokens to anyone any more.
            }
        }

        await using var transaction = await connection.BeginTransactionAsync(ct);
        var sessionIds = (await connection.QueryAsync<string>(new CommandDefinition(
            "SELECT id FROM casebox.sessions WHERE org_id = @Org AND person = ANY(@Subjects)",
            new { Org = orgId, Subjects = subjects.ToArray() }, transaction, cancellationToken: ct))).ToArray();
        await connection.ExecuteAsync(new CommandDefinition(
            """
            DELETE FROM casebox.session_events WHERE org_id = @Org AND session_id = ANY(@Ids);
            DELETE FROM casebox.session_metrics WHERE org_id = @Org AND agent || ':' || session_id = ANY(@Ids);
            DELETE FROM casebox.sessions WHERE org_id = @Org AND id = ANY(@Ids);
            """,
            new { Org = orgId, Ids = sessionIds }, transaction, cancellationToken: ct));
        var result = new ErasureResult(subjects.Count, sessionIds.Length);
        await store.UseTransaction(transaction).Append(Organisation.StreamId, ExpectedVersion.Any, [new OrgEvents.ErasurePerformed(result.Subjects, result.Sessions)]);
        await transaction.CommitAsync(ct);

        // Deedbox erases the subject keys in every period whose secret still exists.
        foreach (var id in identities)
            await admin.EraseIdentityAsync(id, orgId, ct);
        return result;
    }
}

public static class PrivacyEndpoints
{
    public sealed record EraseRequest(string Identity);

    public static void MapPrivacy(this RouteGroupBuilder api)
    {
        api.MapPost("/privacy/erasures", async (EraseRequest body, Erasure erasure, HttpContext http) =>
            Results.Ok(await erasure.EraseAsync(body.Identity ?? "", http.RequestAborted)))
            .WithTags("Privacy").RequireAuthorization(Policies.Admin);
    }
}
