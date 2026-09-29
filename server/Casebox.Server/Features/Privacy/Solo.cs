using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Privacy;

// The solo trial: while an organisation has one account and one person in its data, that person
// sees their own data without k, because nobody else's data is there to protect. The first second
// account or second person (in any one pseudonym period, mapped or not) ends solo with an
// org.solo_ended event, and k applies from then on, even if the second person is later erased.
// Only views inside Casebox use this; what leaves Casebox, such as a proposal's pull request,
// always uses k.
public sealed class Solo(NpgsqlDataSource db, DeedboxContext context, IEventStore store)
{
    public async Task<KView> ViewAsync(CancellationToken ct)
    {
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId, ct);
        return await ViewAsync(org, ct);
    }

    public async Task<KView> ViewAsync(Organisation org, CancellationToken ct)
    {
        if (org.SoloEnded)
            return KView.Of(org.Settings.K);
        await using var connection = await db.OpenConnectionAsync(ct);
        var args = new { Org = context.TenantId };
        var accounts = await connection.ExecuteScalarAsync<int>(
            new CommandDefinition(
                "SELECT count(*)::int FROM casebox.accounts WHERE org_id = @Org",
                args,
                cancellationToken: ct
            )
        );
        var seen = (
            await connection.QueryAsync<(string Person, string Period)>(
                new CommandDefinition(
                    """
                    SELECT DISTINCT person, period FROM casebox.sessions WHERE org_id = @Org
                    UNION
                    SELECT DISTINCT person, period FROM casebox.steering_facts WHERE org_id = @Org
                    """,
                    args,
                    cancellationToken: ct
                )
            )
        ).ToList();
        var authors = await connection.QueryAsync<(string Person, DateTime CreatedAt)>(
            new CommandDefinition(
                """
                SELECT DISTINCT snapshot->'author'->>'token', (snapshot->>'createdAt')::timestamptz
                FROM casebox.pull_requests
                WHERE org_id = @Org AND snapshot->'author'->>'token' IS NOT NULL
                  AND NOT coalesce((snapshot->'author'->>'bot')::boolean, false)
                """,
                args,
                cancellationToken: ct
            )
        );
        seen.AddRange(
            authors.Select(a =>
                (
                    a.Person,
                    Capture.Identities.PeriodOf(
                        org.Settings.PseudonymPeriod,
                        new DateTimeOffset(DateTime.SpecifyKind(a.CreatedAt, DateTimeKind.Utc))
                    )
                )
            )
        );
        var people = KRule.People(seen.Select(s => new Person(s.Person, true, s.Period)));
        if (accounts <= 1 && people <= 1)
            return new KView(org.Settings.K, true);
        await store.Execute<Organisation>(
            Organisation.StreamId,
            state => OrgDecider.EndSolo(state, accounts, people),
            ct
        );
        return KView.Of(org.Settings.K);
    }
}
