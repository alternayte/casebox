using Dapper;
using Microsoft.Extensions.Diagnostics.HealthChecks;
using Microsoft.Extensions.Options;
using Npgsql;

namespace Casebox.Server.Features.Health;

public sealed class PostgresHealthCheck(NpgsqlDataSource dataSource) : IHealthCheck
{
    public async Task<HealthCheckResult> CheckHealthAsync(
        HealthCheckContext context,
        CancellationToken ct = default
    )
    {
        try
        {
            await using var connection = await dataSource.OpenConnectionAsync(ct);
            await connection.ExecuteScalarAsync<int>(
                new CommandDefinition("SELECT 1", cancellationToken: ct)
            );
            return HealthCheckResult.Healthy();
        }
        catch (NpgsqlException e)
        {
            return HealthCheckResult.Unhealthy("The database is unreachable.", e);
        }
    }
}

public sealed class QueueBoxHealthCheck(HttpClient http, IOptions<CaseboxOptions> options)
    : IHealthCheck
{
    public async Task<HealthCheckResult> CheckHealthAsync(
        HealthCheckContext context,
        CancellationToken ct = default
    )
    {
        try
        {
            using var response = await http.GetAsync(options.Value.QueueBox.HealthUrl, ct);
            return response.IsSuccessStatusCode
                ? HealthCheckResult.Healthy()
                : HealthCheckResult.Unhealthy($"QueueBox answered {(int)response.StatusCode}.");
        }
        catch (HttpRequestException e)
        {
            return HealthCheckResult.Unhealthy("QueueBox is unreachable.", e);
        }
    }
}

// Jobs are waiting but no worker was seen in the last 10 minutes. The server itself is fine, so
// this degrades health instead of failing readiness.
public sealed class WorkersHealthCheck(NpgsqlDataSource dataSource, TimeProvider clock)
    : IHealthCheck
{
    public async Task<HealthCheckResult> CheckHealthAsync(
        HealthCheckContext context,
        CancellationToken ct = default
    )
    {
        var now = clock.GetUtcNow();
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var stranded = await connection.ExecuteScalarAsync<long>(
            new CommandDefinition(
                """
                SELECT count(*) FROM casebox.jobs j
                WHERE j.status = 'queued' AND j.available_at < @Waiting
                  AND NOT EXISTS (SELECT 1 FROM casebox.workers w WHERE w.org_id = j.org_id AND w.last_seen_at > @Seen)
                """,
                new
                {
                    Waiting = now - TimeSpan.FromMinutes(1),
                    Seen = now - TimeSpan.FromMinutes(10),
                },
                cancellationToken: ct
            )
        );
        return stranded == 0
            ? HealthCheckResult.Healthy()
            : HealthCheckResult.Degraded(
                $"{stranded} jobs are waiting and no worker was seen in the last 10 minutes."
            );
    }
}
