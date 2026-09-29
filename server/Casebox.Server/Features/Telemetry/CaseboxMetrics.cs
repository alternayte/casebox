using System.Collections.Concurrent;
using System.Diagnostics.Metrics;
using Dapper;
using Npgsql;

namespace Casebox.Server.Features.Telemetry;

// Casebox's own meter (docs/specs/operations.md, Telemetry). Counters and histograms are recorded
// where things happen; the gauges read a snapshot taken every 30 seconds, so a scrape never
// queries the database. No metric carries a person, a repository or a workspace.
public static class CaseboxMetrics
{
    public const string Name = "Casebox";

    public static readonly Meter Meter = new(Name);

    public static readonly Histogram<double> IngestLag = Meter.CreateHistogram<double>(
        "casebox.ingest.lag",
        "s",
        "Receipt time minus the newest event's time, per uploaded batch."
    );

    public static readonly Counter<long> LeaseExpiries = Meter.CreateCounter<long>(
        "casebox.jobs.lease_expiries",
        "{lease}",
        "Job leases that expired and were leased again."
    );

    // What each CLI reported as not yet acknowledged, by token, with when it reported it.
    private static readonly ConcurrentDictionary<string, (long Events, DateTimeOffset At)> Spools =
        new();

    public static void ReportSpool(string token, long events, DateTimeOffset at) =>
        Spools[token] = (Math.Max(0, events), at);

    internal static volatile Snapshot Current = Snapshot.Empty;

    static CaseboxMetrics()
    {
        Meter.CreateObservableGauge(
            "casebox.spool.backlog",
            () =>
            {
                var since = DateTimeOffset.UtcNow.AddHours(-1);
                return Spools.Values.Where(s => s.At > since).Sum(s => s.Events);
            },
            "{event}",
            "Events the CLIs reported as not yet uploaded, over the last hour."
        );
        Meter.CreateObservableGauge(
            "casebox.jobs.queued",
            () =>
                Current.Queued.Select(q => new Measurement<long>(
                    q.Count,
                    new KeyValuePair<string, object?>("kind", q.Kind)
                )),
            "{job}",
            "Queued jobs by kind."
        );
        Meter.CreateObservableGauge(
            "casebox.jobs.oldest",
            () =>
                Current.Queued.Select(q => new Measurement<double>(
                    q.OldestSeconds,
                    new KeyValuePair<string, object?>("kind", q.Kind)
                )),
            "s",
            "How long the oldest queued job of a kind has waited."
        );
        Meter.CreateObservableGauge(
            "casebox.workers.seen",
            () => Current.Workers,
            "{worker}",
            "Workers seen in the last 10 minutes."
        );
        Meter.CreateObservableGauge(
            "casebox.steering.unclassified",
            () =>
                Current.Orgs.Select(o => new Measurement<double>(
                    o.Unclassified,
                    new KeyValuePair<string, object?>("org", o.Org)
                )),
            "1",
            "Share of the last 30 days' interventions the classifier left unclassified."
        );
    }

    public sealed record Queue(string Kind, long Count, double OldestSeconds);

    public sealed record OrgFigures(string Org, double Unclassified);

    public sealed record Snapshot(
        IReadOnlyList<Queue> Queued,
        long Workers,
        IReadOnlyList<OrgFigures> Orgs
    )
    {
        public static Snapshot Empty { get; } = new([], 0, []);
    }
}

// Takes the gauges' snapshot every 30 seconds.
public sealed class MetricsSnapshot(
    NpgsqlDataSource db,
    TimeProvider clock,
    ILogger<MetricsSnapshot> logger
) : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                CaseboxMetrics.Current = await TakeAsync(stoppingToken);
            }
            catch (Exception e) when (e is not OperationCanceledException)
            {
                logger.LogWarning(e, "The metrics snapshot failed; the gauges keep the last one.");
            }
            await Task.Delay(TimeSpan.FromSeconds(30), clock, stoppingToken);
        }
    }

    public async Task<CaseboxMetrics.Snapshot> TakeAsync(CancellationToken ct)
    {
        var now = clock.GetUtcNow();
        await using var connection = await db.OpenConnectionAsync(ct);
        var queued = (
            await connection.QueryAsync<(string Kind, long Count, DateTime Oldest)>(
                new CommandDefinition(
                    "SELECT kind, count(*), min(available_at) FROM casebox.jobs WHERE status = 'queued' GROUP BY kind",
                    cancellationToken: ct
                )
            )
        )
            .Select(q => new CaseboxMetrics.Queue(
                q.Kind,
                q.Count,
                Math.Max(
                    0,
                    (
                        now - new DateTimeOffset(DateTime.SpecifyKind(q.Oldest, DateTimeKind.Utc))
                    ).TotalSeconds
                )
            ))
            .ToList();
        var workers = await connection.ExecuteScalarAsync<long>(
            new CommandDefinition(
                "SELECT count(*) FROM casebox.workers WHERE last_seen_at > @Since",
                new { Since = now - TimeSpan.FromMinutes(10) },
                cancellationToken: ct
            )
        );
        var figures = await connection.QueryAsync<(string Org, long Unclassified, long Total)>(
            new CommandDefinition(
                """
                SELECT o.org_id,
                       (SELECT count(*) FROM casebox.steering_facts f WHERE f.org_id = o.org_id AND f.at >= @Since AND f.status = 'unclassified'),
                       (SELECT count(*) FROM casebox.steering_facts f WHERE f.org_id = o.org_id AND f.at >= @Since AND f.status <> 'pending')
                FROM (SELECT DISTINCT org_id FROM casebox.accounts) o
                """,
                new { Since = now - TimeSpan.FromDays(30) },
                cancellationToken: ct
            )
        );
        var orgs = figures
            .Select(f => new CaseboxMetrics.OrgFigures(
                f.Org,
                f.Total == 0 ? 0 : (double)f.Unclassified / f.Total
            ))
            .ToList();
        return new(queued, workers, orgs);
    }
}
