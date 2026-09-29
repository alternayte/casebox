using System.Security.Cryptography;
using System.Text.RegularExpressions;
using Amazon.S3;
using Amazon.S3.Model;
using Dapper;
using Npgsql;
using ZstdSharp;

namespace Casebox.Server.Features.Blobs;

public sealed record BlobContent(string ContentType, long Size, byte[] Data);

// Content-addressed blobs: the key is the SHA-256 of the uncompressed bytes. Every blob has an
// index row in casebox.blobs, scoped by organisation; the bytes are zstd-compressed, either in
// that row or in an S3-compatible bucket.
public abstract partial class BlobStore(NpgsqlDataSource dataSource, TimeProvider clock)
{
    public const long MaxSize = 64 * 1024 * 1024;

    [GeneratedRegex("^[0-9a-f]{64}$")]
    private static partial Regex HashPattern();

    public static bool IsHash(string hash) => HashPattern().IsMatch(hash);

    public static string HashOf(byte[] data) => Convert.ToHexStringLower(SHA256.HashData(data));

    // Stores the blob once. Returns false when it already existed.
    public async Task<bool> PutAsync(
        string orgId,
        string hash,
        string contentType,
        byte[] data,
        CancellationToken ct
    )
    {
        if (!IsHash(hash))
            throw new DomainException("A blob hash is 64 lower-case hex characters.");
        if (HashOf(data) != hash)
            throw new DomainException("The content does not match its hash.");

        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var exists = await connection.ExecuteScalarAsync<bool>(
            new CommandDefinition(
                "SELECT EXISTS (SELECT 1 FROM casebox.blobs WHERE org_id = @Org AND hash = @Hash)",
                new { Org = orgId, Hash = hash },
                cancellationToken: ct
            )
        );
        if (exists)
            return false;

        using var compressor = new Compressor(3);
        var compressed = compressor.Wrap(data).ToArray();
        var inline = await WriteBytesAsync(orgId, hash, compressed, ct);
        var inserted = await connection.ExecuteAsync(
            new CommandDefinition(
                """
                INSERT INTO casebox.blobs (org_id, hash, size, content_type, data, created_at)
                VALUES (@Org, @Hash, @Size, @ContentType, @Data, @Now)
                ON CONFLICT (org_id, hash) DO NOTHING
                """,
                new
                {
                    Org = orgId,
                    Hash = hash,
                    Size = (long)data.Length,
                    ContentType = contentType,
                    Data = inline,
                    Now = clock.GetUtcNow(),
                },
                cancellationToken: ct
            )
        );
        return inserted == 1;
    }

    public async Task<BlobContent?> GetAsync(string orgId, string hash, CancellationToken ct)
    {
        if (!IsHash(hash))
            return null;
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleOrDefaultAsync<(
            long Size,
            string ContentType,
            byte[]? Data
        )?>(
            new CommandDefinition(
                "SELECT size, content_type, data FROM casebox.blobs WHERE org_id = @Org AND hash = @Hash",
                new { Org = orgId, Hash = hash },
                cancellationToken: ct
            )
        );
        if (row is not { } r)
            return null;

        var compressed = r.Data ?? await ReadBytesAsync(orgId, hash, ct);
        using var decompressor = new Decompressor();
        return new BlobContent(r.ContentType, r.Size, decompressor.Unwrap(compressed).ToArray());
    }

    // Writes the compressed bytes to the backing store; returns them when they belong in the index row.
    protected abstract Task<byte[]?> WriteBytesAsync(
        string orgId,
        string hash,
        byte[] compressed,
        CancellationToken ct
    );

    protected abstract Task<byte[]> ReadBytesAsync(string orgId, string hash, CancellationToken ct);
}

public sealed class PostgresBlobStore(NpgsqlDataSource dataSource, TimeProvider clock)
    : BlobStore(dataSource, clock)
{
    protected override Task<byte[]?> WriteBytesAsync(
        string orgId,
        string hash,
        byte[] compressed,
        CancellationToken ct
    ) => Task.FromResult<byte[]?>(compressed);

    protected override Task<byte[]> ReadBytesAsync(
        string orgId,
        string hash,
        CancellationToken ct
    ) => throw new InvalidOperationException($"Blob {hash} has no bytes in Postgres.");
}

public sealed class S3BlobStore(
    NpgsqlDataSource dataSource,
    TimeProvider clock,
    IAmazonS3 s3,
    string bucket
) : BlobStore(dataSource, clock)
{
    protected override async Task<byte[]?> WriteBytesAsync(
        string orgId,
        string hash,
        byte[] compressed,
        CancellationToken ct
    )
    {
        using var body = new MemoryStream(compressed);
        await s3.PutObjectAsync(
            new PutObjectRequest
            {
                BucketName = bucket,
                Key = Key(orgId, hash),
                InputStream = body,
                ContentType = "application/zstd",
            },
            ct
        );
        return null;
    }

    protected override async Task<byte[]> ReadBytesAsync(
        string orgId,
        string hash,
        CancellationToken ct
    )
    {
        using var response = await s3.GetObjectAsync(bucket, Key(orgId, hash), ct);
        using var buffer = new MemoryStream();
        await response.ResponseStream.CopyToAsync(buffer, ct);
        return buffer.ToArray();
    }

    private static string Key(string orgId, string hash) => $"{orgId}/{hash[..2]}/{hash}.zst";
}
