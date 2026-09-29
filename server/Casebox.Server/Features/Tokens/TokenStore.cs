using System.Security.Cryptography;
using System.Text;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Tokens;

public enum TokenKind { Worker, Ingest }

public sealed record TokenInfo(string Id, TokenKind Kind, string Name, string CreatedBy, DateTimeOffset CreatedAt, DateTimeOffset? LastUsedAt, DateTimeOffset? RevokedAt);

public sealed record IssuedToken(TokenInfo Info, string Secret);

public sealed record AuthenticatedToken(string Id, string OrgId, TokenKind Kind);

// Worker and ingest tokens. A token is shown once, when it is issued; only its SHA-256 is stored.
// Issuing and revoking are audited as org events in the same transaction.
public sealed class TokenStore(NpgsqlDataSource dataSource, TimeProvider clock)
{
    public async Task<IssuedToken> IssueAsync(string orgId, TokenKind kind, string name, string createdBy, IEventStore store, CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(name) || name.Length > 100) throw new DomainException("A token name is 1 to 100 characters.");
        var id = Ids.New();
        var secret = $"cbx_{KindName(kind)}_{Convert.ToHexStringLower(RandomNumberGenerator.GetBytes(32))}";
        var now = clock.GetUtcNow();

        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        await connection.ExecuteAsync(new CommandDefinition(
            """
            INSERT INTO casebox.api_tokens (id, org_id, kind, name, token_hash, created_by, created_at)
            VALUES (@Id, @Org, @Kind, @Name, @Hash, @CreatedBy, @Now)
            """,
            new { Id = id, Org = orgId, Kind = KindName(kind), Name = name.Trim(), Hash = Hash(secret), CreatedBy = createdBy, Now = now },
            transaction, cancellationToken: ct));
        await store.UseTransaction(transaction).Append(Organisation.StreamId, ExpectedVersion.Any, [new OrgEvents.TokenIssued(id, KindName(kind), name.Trim())]);
        await transaction.CommitAsync(ct);

        return new IssuedToken(new TokenInfo(id, kind, name.Trim(), createdBy, now, null, null), secret);
    }

    public async Task RevokeAsync(string orgId, string tokenId, IEventStore store, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        var rows = (await connection.QueryAsync<DateTime?>(new CommandDefinition(
            "SELECT revoked_at FROM casebox.api_tokens WHERE org_id = @Org AND id = @Id FOR UPDATE",
            new { Org = orgId, Id = tokenId }, transaction, cancellationToken: ct))).ToList();
        if (rows.Count == 0) throw new NotFoundException("The token does not exist.");
        if (rows[0] is not null) return;

        await connection.ExecuteAsync(new CommandDefinition(
            "UPDATE casebox.api_tokens SET revoked_at = @Now WHERE org_id = @Org AND id = @Id",
            new { Org = orgId, Id = tokenId, Now = clock.GetUtcNow() }, transaction, cancellationToken: ct));
        await store.UseTransaction(transaction).Append(Organisation.StreamId, ExpectedVersion.Any, [new OrgEvents.TokenRevoked(tokenId)]);
        await transaction.CommitAsync(ct);
    }

    public async Task<IReadOnlyList<TokenInfo>> ListAsync(string orgId, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var rows = await connection.QueryAsync<Row>(new CommandDefinition(
            """
            SELECT id, kind, name, created_by, created_at, last_used_at, revoked_at
            FROM casebox.api_tokens WHERE org_id = @Org ORDER BY created_at
            """,
            new { Org = orgId }, cancellationToken: ct));
        return rows.Select(r => new TokenInfo(r.Id, ParseKind(r.Kind), r.Name, r.CreatedBy, Utc(r.CreatedAt), Utc(r.LastUsedAt), Utc(r.RevokedAt))).ToList();
    }

    public async Task<AuthenticatedToken?> AuthenticateAsync(string secret, CancellationToken ct)
    {
        if (!secret.StartsWith("cbx_", StringComparison.Ordinal)) return null;
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleOrDefaultAsync<(string Id, string OrgId, string Kind)?>(new CommandDefinition(
            """
            UPDATE casebox.api_tokens
            SET last_used_at = CASE WHEN last_used_at IS NULL OR last_used_at < @Now - interval '1 minute' THEN @Now ELSE last_used_at END
            WHERE token_hash = @Hash AND revoked_at IS NULL
            RETURNING id, org_id, kind
            """,
            new { Hash = Hash(secret), Now = clock.GetUtcNow() }, cancellationToken: ct));
        return row is { } r ? new AuthenticatedToken(r.Id, r.OrgId, ParseKind(r.Kind)) : null;
    }

    public static string KindName(TokenKind kind) => kind.ToString().ToLowerInvariant();

    private static TokenKind ParseKind(string kind) => Enum.Parse<TokenKind>(kind, ignoreCase: true);

    private static byte[] Hash(string secret) => SHA256.HashData(Encoding.UTF8.GetBytes(secret));

    private static DateTimeOffset Utc(DateTime value) => new(DateTime.SpecifyKind(value, DateTimeKind.Utc));

    private static DateTimeOffset? Utc(DateTime? value) => value is { } v ? Utc(v) : null;

    private sealed record Row(string Id, string Kind, string Name, string CreatedBy, DateTime CreatedAt, DateTime? LastUsedAt, DateTime? RevokedAt);
}
