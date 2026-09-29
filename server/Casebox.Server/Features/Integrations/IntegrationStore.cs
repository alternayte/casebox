using System.Text.Json;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Microsoft.AspNetCore.DataProtection;
using Npgsql;

namespace Casebox.Server.Features.Integrations;

// Issues: GitHub Issues are work items too.
public sealed record GitHubSettings(
    string Mode,
    long? AppId = null,
    long? InstallationId = null,
    bool Issues = false
);

// The secret half of the GitHub settings: a token, or an App's private key and webhook secret.
public sealed record GitHubSecret(
    string? Token = null,
    string? PrivateKey = null,
    string? WebhookSecret = null
);

public sealed record JiraSettings(string Url, IReadOnlyList<string> Projects);

// Azure DevOps Server: the collection URLs, and one personal access token per collection.
public sealed record AzureDevOpsSettings(IReadOnlyList<string> Collections);

public sealed record AzureDevOpsSecret(IReadOnlyDictionary<string, string> Tokens);

public sealed record IntegrationStatus(
    string Kind,
    JsonElement Config,
    JsonElement? LastPoll,
    DateTimeOffset UpdatedAt
);

// Integration settings. The secret is Data Protection ciphertext and never leaves the server.
public sealed class IntegrationStore(
    NpgsqlDataSource dataSource,
    IDataProtectionProvider protection,
    TimeProvider clock
)
{
    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);
    private readonly IDataProtector _protector = protection.CreateProtector("casebox.integrations");

    public async Task SaveAsync<TConfig, TSecret>(
        string orgId,
        string kind,
        TConfig config,
        TSecret secret,
        IEventStore store,
        CancellationToken ct
    )
    {
        var cipher = _protector.Protect(JsonSerializer.SerializeToUtf8Bytes(secret, Json));
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.integrations (org_id, kind, config, secret, updated_at) VALUES (@Org, @Kind, @Config::jsonb, @Secret, @Now)
                ON CONFLICT (org_id, kind) DO UPDATE SET config = EXCLUDED.config, secret = EXCLUDED.secret, updated_at = EXCLUDED.updated_at
                """,
                new
                {
                    Org = orgId,
                    Kind = kind,
                    Config = JsonSerializer.Serialize(config, Json),
                    Secret = cipher,
                    Now = clock.GetUtcNow(),
                },
                transaction,
                cancellationToken: ct
            )
        );
        await store
            .UseTransaction(transaction)
            .Append(
                Organisation.StreamId,
                ExpectedVersion.Any,
                [new OrgEvents.IntegrationConfigured(kind, true)]
            );
        await transaction.CommitAsync(ct);
    }

    public async Task RemoveAsync(
        string orgId,
        string kind,
        IEventStore store,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var removed = await connection.ExecuteAsync(
            new CommandDefinition(
                "DELETE FROM casebox.integrations WHERE org_id = @Org AND kind = @Kind",
                new { Org = orgId, Kind = kind },
                transaction,
                cancellationToken: ct
            )
        );
        if (removed == 0)
            throw new NotFoundException($"No {kind} integration is connected.");
        await store
            .UseTransaction(transaction)
            .Append(
                Organisation.StreamId,
                ExpectedVersion.Any,
                [new OrgEvents.IntegrationConfigured(kind, false)]
            );
        await transaction.CommitAsync(ct);
    }

    public async Task<(TConfig Config, TSecret Secret)?> GetAsync<TConfig, TSecret>(
        string orgId,
        string kind,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleOrDefaultAsync<(string Config, byte[] Secret)?>(
            new CommandDefinition(
                "SELECT config::text, secret FROM casebox.integrations WHERE org_id = @Org AND kind = @Kind",
                new { Org = orgId, Kind = kind },
                cancellationToken: ct
            )
        );
        if (row is not { } r)
            return null;
        return (
            JsonSerializer.Deserialize<TConfig>(r.Config, Json)!,
            JsonSerializer.Deserialize<TSecret>(_protector.Unprotect(r.Secret), Json)!
        );
    }

    // The settings without the secret, for code that needs no credential.
    public async Task<TConfig?> ConfigAsync<TConfig>(
        string orgId,
        string kind,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var config = await connection.QuerySingleOrDefaultAsync<string>(
            new CommandDefinition(
                "SELECT config::text FROM casebox.integrations WHERE org_id = @Org AND kind = @Kind",
                new { Org = orgId, Kind = kind },
                cancellationToken: ct
            )
        );
        return config is null ? default : JsonSerializer.Deserialize<TConfig>(config, Json);
    }

    public async Task<IReadOnlyList<IntegrationStatus>> ListAsync(
        string orgId,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var rows = await connection.QueryAsync<(
            string Kind,
            string Config,
            string? LastPoll,
            DateTime UpdatedAt
        )>(
            new CommandDefinition(
                "SELECT kind, config::text, last_poll::text, updated_at FROM casebox.integrations WHERE org_id = @Org ORDER BY kind",
                new { Org = orgId },
                cancellationToken: ct
            )
        );
        return rows.Select(r => new IntegrationStatus(
                r.Kind,
                JsonDocument.Parse(r.Config).RootElement.Clone(),
                r.LastPoll is null ? null : JsonDocument.Parse(r.LastPoll).RootElement.Clone(),
                new DateTimeOffset(DateTime.SpecifyKind(r.UpdatedAt, DateTimeKind.Utc))
            ))
            .ToList();
    }

    // Every organisation with this kind of integration, for the pollers.
    public async Task<IReadOnlyList<string>> OrgsWithAsync(string kind, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        return (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    "SELECT org_id FROM casebox.integrations WHERE kind = @Kind",
                    new { Kind = kind },
                    cancellationToken: ct
                )
            )
        ).ToList();
    }

    public async Task RecordPollAsync(
        string orgId,
        string kind,
        object result,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await connection.ExecuteAsync(
            new CommandDefinition(
                "UPDATE casebox.integrations SET last_poll = @Result::jsonb WHERE org_id = @Org AND kind = @Kind",
                new
                {
                    Org = orgId,
                    Kind = kind,
                    Result = JsonSerializer.Serialize(result, Json),
                },
                cancellationToken: ct
            )
        );
    }

    public async Task<string?> CursorAsync(string orgId, string source, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        return await connection.QuerySingleOrDefaultAsync<string>(
            new CommandDefinition(
                "SELECT cursor FROM casebox.integration_cursors WHERE org_id = @Org AND source = @Source",
                new { Org = orgId, Source = source },
                cancellationToken: ct
            )
        );
    }

    public async Task SetCursorAsync(
        string orgId,
        string source,
        string cursor,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.integration_cursors (org_id, source, cursor, updated_at) VALUES (@Org, @Source, @Cursor, @Now)
                ON CONFLICT (org_id, source) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = EXCLUDED.updated_at
                """,
                new
                {
                    Org = orgId,
                    Source = source,
                    Cursor = cursor,
                    Now = clock.GetUtcNow(),
                },
                cancellationToken: ct
            )
        );
    }
}
