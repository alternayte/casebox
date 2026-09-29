using System.Data.Common;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Dapper;

namespace Casebox.Server.Features.Ci;

// One casebox ci request (docs/specs/harness-ci.md): a nightly baseline or one pull request's
// smoke run in one workspace. It links the request to its evaluation, and holds the pull request
// the comment goes to.
public sealed record CiRun(
    string Id,
    string Kind,
    string? Workspace,
    string Repo,
    int? Number,
    string? HeadSha,
    string? BaseSha,
    string? ServerUrl,
    string? EvaluationId,
    string Status,
    string? Message,
    string Request,
    string CreatedBy,
    // UTC; Npgsql reads timestamptz as DateTime, and Dapper maps a record by exact types.
    DateTime CreatedAt
);

public static class CiRuns
{
    public const string Baseline = "baseline";
    public const string PullRequest = "pull_request";
    public const string Proposer = "proposer";

    public const string Resolving = "resolving";
    public const string Skipped = "skipped";
    public const string Failed = "failed";
    public const string Started = "started";

    private const string Columns =
        "id, kind, workspace, repo, number, head_sha, base_sha, server_url, evaluation_id, status, message, request::text AS request, created_by, created_at";

    public static Task InsertAsync(
        DbConnection connection,
        DbTransaction? transaction,
        string org,
        CiRun run,
        CancellationToken ct
    ) =>
        connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.ci_runs (org_id, id, kind, workspace, repo, number, head_sha, base_sha, server_url, evaluation_id, status, message, request, created_by, created_at, updated_at)
                VALUES (@Org, @Id, @Kind, @Workspace, @Repo, @Number, @HeadSha, @BaseSha, @ServerUrl, @EvaluationId, @Status, @Message, @Request::jsonb, @CreatedBy, @CreatedAt, @CreatedAt)
                """,
                new
                {
                    Org = org,
                    run.Id,
                    run.Kind,
                    run.Workspace,
                    run.Repo,
                    run.Number,
                    run.HeadSha,
                    run.BaseSha,
                    run.ServerUrl,
                    run.EvaluationId,
                    run.Status,
                    run.Message,
                    run.Request,
                    run.CreatedBy,
                    run.CreatedAt,
                },
                transaction,
                cancellationToken: ct
            )
        );

    public static Task UpdateAsync(
        DbConnection connection,
        DbTransaction? transaction,
        string org,
        string id,
        string status,
        string? evaluationId,
        string? message,
        DateTimeOffset now,
        CancellationToken ct
    ) =>
        connection.ExecuteAsync(
            new CommandDefinition(
                """
                UPDATE casebox.ci_runs SET status = @Status, evaluation_id = coalesce(@Evaluation, evaluation_id), message = @Message, updated_at = @Now
                WHERE org_id = @Org AND id = @Id
                """,
                new
                {
                    Org = org,
                    Id = id,
                    Status = status,
                    Evaluation = evaluationId,
                    Message = message,
                    Now = now,
                },
                transaction,
                cancellationToken: ct
            )
        );

    public static Task<CiRun?> GetAsync(
        DbConnection connection,
        DbTransaction? transaction,
        string org,
        string id,
        CancellationToken ct
    ) =>
        connection.QuerySingleOrDefaultAsync<CiRun>(
            new CommandDefinition(
                $"SELECT {Columns} FROM casebox.ci_runs WHERE org_id = @Org AND id = @Id",
                new { Org = org, Id = id },
                transaction,
                cancellationToken: ct
            )
        );

    public static Task<CiRun?> ForEvaluationAsync(
        DbConnection connection,
        string org,
        string evaluationId,
        CancellationToken ct
    ) =>
        connection.QuerySingleOrDefaultAsync<CiRun>(
            new CommandDefinition(
                $"SELECT {Columns} FROM casebox.ci_runs WHERE org_id = @Org AND evaluation_id = @Evaluation",
                new { Org = org, Evaluation = evaluationId },
                cancellationToken: ct
            )
        );

    // A ci token acts only on the CI runs it created.
    public static async Task<bool> OwnedByAsync(
        DbConnection connection,
        string org,
        string id,
        string tokenId,
        CancellationToken ct
    ) =>
        await connection.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                "SELECT EXISTS (SELECT 1 FROM casebox.ci_runs WHERE org_id = @Org AND id = @Id AND created_by = @Token)",
                new
                {
                    Org = org,
                    Id = id,
                    Token = $"token:{tokenId}",
                },
                cancellationToken: ct
            )
        );

    // A ci token's inline worker may act on a job only when the job belongs to one of its CI runs.
    public static async Task<bool> JobOwnedByAsync(
        DbConnection connection,
        string org,
        string jobId,
        string tokenId,
        CancellationToken ct
    ) =>
        await connection.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                """
                SELECT EXISTS (
                    SELECT 1 FROM casebox.jobs j JOIN casebox.ci_runs c ON c.org_id = j.org_id AND c.id = j.payload->>'ciRun'
                    WHERE j.org_id = @Org AND j.id = @Job AND c.created_by = @Token)
                """,
                new
                {
                    Org = org,
                    Job = jobId,
                    Token = $"token:{tokenId}",
                },
                cancellationToken: ct
            )
        );

    // A ci token reads a blob only when a job of one of its CI runs names it, or names the oracle
    // that names it (the held-out test patch).
    public static async Task<bool> BlobReadableAsync(
        DbConnection connection,
        string org,
        string hash,
        string tokenId,
        Func<string, Task<byte[]?>> blob,
        CancellationToken ct
    )
    {
        var payloads = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    """
                    SELECT j.payload::text FROM casebox.jobs j JOIN casebox.ci_runs c ON c.org_id = j.org_id AND c.id = j.payload->>'ciRun'
                    WHERE j.org_id = @Org AND c.created_by = @Token AND j.status IN ('queued', 'leased')
                    """,
                    new { Org = org, Token = $"token:{tokenId}" },
                    cancellationToken: ct
                )
            )
        ).ToList();
        if (payloads.Any(p => p.Contains(hash, StringComparison.Ordinal)))
            return true;
        foreach (var oracle in payloads.Select(OracleOf).OfType<string>().Distinct())
        {
            if (await blob(oracle) is { } bytes && Encoding.UTF8.GetString(bytes).Contains(hash))
                return true;
        }
        return false;
    }

    private static string? OracleOf(string payload)
    {
        using var document = JsonDocument.Parse(payload);
        return
            document.RootElement.TryGetProperty("oracle", out var o)
            && o.ValueKind == JsonValueKind.String
            ? o.GetString()
            : null;
    }

    // "github.com/acme/api" becomes "acme/api", the path GitHub's API uses.
    public static string ApiPath(string repo)
    {
        var parts = repo.Split('/');
        return parts.Length >= 3 ? $"{parts[^2]}/{parts[^1]}" : repo;
    }

    // Stable order of a pull request's cases: every push of one pull request gets the same cases.
    public static string Order(string repo, int number, string caseId) =>
        Convert.ToHexStringLower(
            SHA256.HashData(Encoding.UTF8.GetBytes($"{repo}#{number}:{caseId}"))
        );
}
