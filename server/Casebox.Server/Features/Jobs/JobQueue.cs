using System.Data.Common;
using System.Text.Json;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Jobs;

public sealed record Job(
    string Id,
    string Kind,
    string IdempotencyKey,
    JsonElement Payload,
    int Attempts,
    int MaxAttempts,
    DateTimeOffset LeaseExpiresAt
);

// What a job kind does with a worker's result: it appends the matching events, with an expected
// version, inside the transaction that also marks the job succeeded.
public interface IJobResultHandler
{
    string Kind { get; }

    Task HandleAsync(JobResult result, CancellationToken ct);
}

// Services are the request's own, with its organisation set, for handlers that tokenize text.
public sealed record JobResult(
    string OrgId,
    Job Job,
    JsonElement Result,
    DbTransaction Transaction,
    IEventStore Store,
    IServiceProvider Services
);

public sealed class JobQueue(
    NpgsqlDataSource dataSource,
    TimeProvider clock,
    IEnumerable<IJobResultHandler> handlers
)
{
    public static readonly TimeSpan Lease = TimeSpan.FromMinutes(5);
    private static readonly TimeSpan RetryBase = TimeSpan.FromSeconds(30);

    private readonly Dictionary<string, IJobResultHandler> _handlers = handlers.ToDictionary(
        h => h.Kind,
        StringComparer.Ordinal
    );

    // Enqueues once per idempotency key; a repeat returns the existing job's ID.
    public async Task<string> EnqueueAsync(
        DbTransaction transaction,
        string orgId,
        string kind,
        string idempotencyKey,
        object payload,
        int maxAttempts = 3,
        CancellationToken ct = default
    )
    {
        if (!_handlers.ContainsKey(kind))
            throw new InvalidOperationException(
                $"No result handler is registered for job kind '{kind}'."
            );
        var now = clock.GetUtcNow();
        return await transaction.Connection!.QuerySingleAsync<string>(
            new CommandDefinition(
                """
                INSERT INTO casebox.jobs (id, org_id, kind, idempotency_key, payload, status, max_attempts, available_at, created_at, updated_at)
                VALUES (@Id, @Org, @Kind, @Key, @Payload::jsonb, 'queued', @MaxAttempts, @Now, @Now, @Now)
                ON CONFLICT (org_id, idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
                RETURNING id
                """,
                new
                {
                    Id = Ids.New(),
                    Org = orgId,
                    Kind = kind,
                    Key = idempotencyKey,
                    Payload = JsonSerializer.Serialize(payload),
                    MaxAttempts = maxAttempts,
                    Now = now,
                },
                transaction,
                cancellationToken: ct
            )
        );
    }

    // Leases the oldest available job of the given kinds. An expired lease makes a job available
    // again; a job whose attempts are used up fails instead.
    public async Task<Job?> LeaseAsync(
        string orgId,
        string workerId,
        string version,
        IReadOnlyCollection<string> kinds,
        CancellationToken ct,
        string? ciRun = null
    )
    {
        var now = clock.GetUtcNow();
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);

        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.workers (org_id, worker_id, version, last_seen_at, kinds) VALUES (@Org, @Worker, @Version, @Now, @Kinds)
                ON CONFLICT (org_id, worker_id) DO UPDATE SET version = EXCLUDED.version, last_seen_at = EXCLUDED.last_seen_at, kinds = EXCLUDED.kinds
                """,
                new
                {
                    Org = orgId,
                    Worker = workerId,
                    Version = version,
                    Now = now,
                    Kinds = kinds.ToArray(),
                },
                transaction,
                cancellationToken: ct
            )
        );

        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.jobs SET status = 'failed', last_error = 'The lease expired on the last attempt.', lease_owner = NULL, lease_expires_at = NULL, updated_at = @Now
                WHERE org_id = @Org AND status = 'leased' AND lease_expires_at < @Now AND attempts >= max_attempts
                """,
                new { Org = orgId, Now = now },
                transaction,
                cancellationToken: ct
            )
        );

        var row = await connection.QuerySingleOrDefaultAsync<LeaseRow>(
            new CommandDefinition(
                """
                WITH next AS (
                    SELECT id, status = 'leased' AS expired FROM casebox.jobs
                    WHERE org_id = @Org AND kind = ANY(@Kinds) AND (@CiRun::text IS NULL OR payload->>'ciRun' = @CiRun)
                      AND ((status = 'queued' AND available_at <= @Now) OR (status = 'leased' AND lease_expires_at < @Now))
                    ORDER BY available_at, id
                    FOR UPDATE SKIP LOCKED
                    LIMIT 1
                )
                UPDATE casebox.jobs j
                SET status = 'leased', lease_owner = @Worker, lease_expires_at = @Expires, attempts = j.attempts + 1, updated_at = @Now
                FROM next WHERE j.id = next.id
                RETURNING j.id, j.kind, j.idempotency_key, j.payload::text AS payload, j.attempts, j.max_attempts, j.lease_expires_at, j.status, j.lease_owner, next.expired
                """,
                new
                {
                    Org = orgId,
                    Kinds = kinds.ToArray(),
                    Worker = workerId,
                    Now = now,
                    Expires = now + Lease,
                    CiRun = ciRun,
                },
                transaction,
                cancellationToken: ct
            )
        );

        await transaction.CommitAsync(ct);
        if (row is { Expired: true })
            Telemetry.CaseboxMetrics.LeaseExpiries.Add(
                1,
                new KeyValuePair<string, object?>("kind", row.Kind)
            );
        return row is null
            ? null
            : new Row(
                row.Id,
                row.Kind,
                row.IdempotencyKey,
                row.Payload,
                row.Attempts,
                row.MaxAttempts,
                row.LeaseExpiresAt,
                row.Status,
                row.LeaseOwner
            ).ToJob();
    }

    public async Task<bool> HeartbeatAsync(
        string orgId,
        string jobId,
        string workerId,
        CancellationToken ct
    )
    {
        var now = clock.GetUtcNow();
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await connection.ExecuteAsync(
            new CommandDefinition(
                "UPDATE casebox.workers SET last_seen_at = @Now WHERE org_id = @Org AND worker_id = @Worker",
                new
                {
                    Org = orgId,
                    Worker = workerId,
                    Now = now,
                },
                cancellationToken: ct
            )
        );
        var updated = await connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.jobs SET lease_expires_at = @Expires, updated_at = @Now
                WHERE org_id = @Org AND id = @Id AND status = 'leased' AND lease_owner = @Worker
                """,
                new
                {
                    Org = orgId,
                    Id = jobId,
                    Worker = workerId,
                    Now = now,
                    Expires = now + Lease,
                },
                cancellationToken: ct
            )
        );
        return updated == 1;
    }

    // Records a result once. A retried post for a job that already succeeded changes nothing.
    public async Task<JobOutcome> CompleteAsync(
        string orgId,
        string jobId,
        string workerId,
        JsonElement result,
        IEventStore store,
        IServiceProvider services,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var row = await LockAsync(connection, transaction, orgId, jobId, ct);
        if (row is null)
            return JobOutcome.NotFound;
        if (row.Status == "succeeded")
            return JobOutcome.Done;
        if (row.Status != "leased" || row.LeaseOwner != workerId)
            return JobOutcome.LeaseLost;

        var handler = _handlers.TryGetValue(row.Kind, out var h)
            ? h
            : throw new InvalidOperationException(
                $"No result handler is registered for job kind '{row.Kind}'."
            );
        // A worker's result can hold a model's words: handlers see it, and it is stored, only redacted
        // and tokenized.
        var (org, _) = await store.Load<Orgs.Organisation>(Orgs.Organisation.StreamId, ct);
        var stored = await services
            .GetRequiredService<Capture.Identities>()
            .TokenizeJsonAsync(
                result,
                Capture.Identities.PeriodOf(org.Settings.PseudonymPeriod, clock.GetUtcNow()),
                ct
            );
        var tokenized = JsonDocument.Parse(stored).RootElement.Clone();
        await handler.HandleAsync(
            new JobResult(
                orgId,
                row.ToJob(),
                tokenized,
                transaction,
                store.UseTransaction(transaction),
                services
            ),
            ct
        );

        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.jobs SET status = 'succeeded', result = @Result::jsonb, lease_owner = NULL, lease_expires_at = NULL, updated_at = @Now
                WHERE id = @Id
                """,
                new
                {
                    Id = jobId,
                    Result = stored,
                    Now = clock.GetUtcNow(),
                },
                transaction,
                cancellationToken: ct
            )
        );
        await transaction.CommitAsync(ct);
        return JobOutcome.Done;
    }

    // A retryable failure waits 30 s × 2^(attempt − 1) and runs again, until the attempts are used up.
    public async Task<JobOutcome> FailAsync(
        string orgId,
        string jobId,
        string workerId,
        string error,
        bool retryable,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var row = await LockAsync(connection, transaction, orgId, jobId, ct);
        if (row is null)
            return JobOutcome.NotFound;
        if (row.Status is "succeeded" or "failed")
            return JobOutcome.Done;
        if (row.Status != "leased" || row.LeaseOwner != workerId)
            return JobOutcome.LeaseLost;

        var now = clock.GetUtcNow();
        var again = retryable && row.Attempts < row.MaxAttempts;
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.jobs
                SET status = @Status, available_at = @Available, last_error = @Error, lease_owner = NULL, lease_expires_at = NULL, updated_at = @Now
                WHERE id = @Id
                """,
                new
                {
                    Id = jobId,
                    Status = again ? "queued" : "failed",
                    Available = now + RetryBase * Math.Pow(2, row.Attempts - 1),
                    Error = error.Length > 4000 ? error[..4000] : error,
                    Now = now,
                },
                transaction,
                cancellationToken: ct
            )
        );
        await transaction.CommitAsync(ct);
        return JobOutcome.Done;
    }

    private static Task<Row?> LockAsync(
        DbConnection connection,
        DbTransaction transaction,
        string orgId,
        string jobId,
        CancellationToken ct
    ) =>
        connection.QuerySingleOrDefaultAsync<Row?>(
            new CommandDefinition(
                """
                SELECT id, kind, idempotency_key, payload::text AS payload, attempts, max_attempts, lease_expires_at, status, lease_owner
                FROM casebox.jobs WHERE org_id = @Org AND id = @Id FOR UPDATE
                """,
                new { Org = orgId, Id = jobId },
                transaction,
                cancellationToken: ct
            )
        );

    // A leased row, and whether its previous lease had expired.
    private sealed record LeaseRow(
        string Id,
        string Kind,
        string IdempotencyKey,
        string Payload,
        int Attempts,
        int MaxAttempts,
        DateTime? LeaseExpiresAt,
        string Status,
        string? LeaseOwner,
        bool Expired
    );

    private sealed record Row(
        string Id,
        string Kind,
        string IdempotencyKey,
        string Payload,
        int Attempts,
        int MaxAttempts,
        DateTime? LeaseExpiresAt,
        string Status,
        string? LeaseOwner
    )
    {
        public Job ToJob() =>
            new(
                Id,
                Kind,
                IdempotencyKey,
                JsonDocument.Parse(Payload).RootElement.Clone(),
                Attempts,
                MaxAttempts,
                LeaseExpiresAt is { } e
                    ? new DateTimeOffset(DateTime.SpecifyKind(e, DateTimeKind.Utc))
                    : default
            );
    }
}

public enum JobOutcome
{
    Done,
    NotFound,
    LeaseLost,
}
