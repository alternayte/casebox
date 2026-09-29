using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Privacy;

// Retention, once an hour. When a pseudonym period leaves the organisation's retention window
// (default 12 months), all its subjects are erased and its secret is destroyed, so nobody can link
// that period's data to a person again. Trace events and telemetry older than the trace
// retention (default 180 days) are deleted.
public sealed class Retention(IServiceScopeFactory scopes, NpgsqlDataSource db, TimeProvider clock, ILogger<Retention> logger) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await RunAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogError(e, "The retention pass failed; it runs again in an hour.");
            }

            await Task.Delay(TimeSpan.FromHours(1), clock, stoppingToken);
        }
    }

    public async Task RunAsync(CancellationToken ct)
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var orgs = await connection.QueryAsync<string>(new CommandDefinition("SELECT DISTINCT org_id FROM casebox.sessions", cancellationToken: ct));
        foreach (var orgId in orgs)
            await RunForAsync(orgId, ct);
    }

    public async Task RunForAsync(string orgId, CancellationToken ct)
    {
        await using var scope = scopes.CreateAsyncScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = orgId;
        context.Metadata = new EventMetadata { Actor = "system:retention" };
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        var admin = scope.ServiceProvider.GetRequiredService<IEventStoreAdmin>();
        var (org, _) = await store.Load<Organisation>(Organisation.StreamId);
        var now = clock.GetUtcNow();

        await using var connection = await db.OpenConnectionAsync(ct);
        var periods = await connection.QueryAsync<string>(new CommandDefinition(
            "SELECT DISTINCT period FROM casebox.sessions WHERE org_id = @Org AND period_retired_at IS NULL",
            new { Org = orgId }, cancellationToken: ct));
        foreach (var period in periods)
        {
            if (Periods.EndOf(period) is not { } end || end.AddMonths(org.Settings.Retention) > now) continue;

            var subjects = (await connection.QueryAsync<string>(new CommandDefinition(
                "SELECT DISTINCT person FROM casebox.sessions WHERE org_id = @Org AND period = @Period",
                new { Org = orgId, Period = period }, cancellationToken: ct))).ToList();
            foreach (var subject in subjects)
                await admin.EraseSubjectAsync(subject, orgId, ct);
            await admin.DestroyPseudonymPeriodAsync(period, orgId, ct);

            await using var transaction = await connection.BeginTransactionAsync(ct);
            await connection.ExecuteAsync(new CommandDefinition(
                "UPDATE casebox.sessions SET period_retired_at = @Now WHERE org_id = @Org AND period = @Period",
                new { Org = orgId, Period = period, Now = now }, transaction, cancellationToken: ct));
            await store.UseTransaction(transaction).Append(Organisation.StreamId, ExpectedVersion.Any, [new OrgEvents.PeriodRetired(period, subjects.Count)]);
            await transaction.CommitAsync(ct);
            logger.LogInformation("Retired pseudonym period {Period}: {Subjects} subjects erased, secret destroyed.", period, subjects.Count);
        }

        var cutoff = now.AddDays(-org.Settings.TraceRetention);
        await connection.ExecuteAsync(new CommandDefinition(
            """
            DELETE FROM casebox.session_events WHERE org_id = @Org AND at < @Cutoff;
            DELETE FROM casebox.session_metrics WHERE org_id = @Org AND at < @Cutoff;
            """,
            new { Org = orgId, Cutoff = cutoff }, cancellationToken: ct));
    }
}
