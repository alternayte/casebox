using System.Text;
using Casebox.Server.Features.Blobs;
using Dapper;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Infrastructure;

// Scans every text, JSON and byte column of every table in the casebox, deedbox and public
// (QueueBox) schemas, and every blob after decompression, for the given strings.
public static class PrivacyScan
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    public static async Task<List<string>> FindAsync(StackFixture stack, string[] needles)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.OpenAsync(Ct);
        var columns = await db.QueryAsync<(
            string Schema,
            string Table,
            string Column,
            string Type
        )>(
            """
            SELECT c.table_schema, c.table_name, c.column_name, c.data_type
            FROM information_schema.columns c JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
            WHERE c.table_schema IN ('casebox', 'deedbox', 'public') AND t.table_type = 'BASE TABLE'
              AND c.data_type IN ('text', 'character varying', 'jsonb', 'json', 'bytea')
            """
        );
        var found = new List<string>();
        foreach (var (schema, table, column, type) in columns)
        {
            var value = type == "bytea" ? $"encode(\"{column}\", 'escape')" : $"\"{column}\"::text";
            var hits = await db.QuerySingleAsync<int>(
                $"SELECT count(*) FROM \"{schema}\".\"{table}\" WHERE lower({value}) LIKE ANY(@Patterns)",
                new { Patterns = needles.Select(n => $"%{n}%").ToArray() }
            );
            if (hits > 0)
                found.Add($"{schema}.{table}.{column}");
        }

        // Blobs are zstd-compressed, so they are scanned after decompression. A row without bytes
        // points at the S3-compatible store.
        var postgres = stack.ServerA.Services.GetRequiredService<BlobStore>();
        using var s3Client = new Amazon.S3.AmazonS3Client(
            new Amazon.Runtime.BasicAWSCredentials(
                StackFixture.S3AccessKey,
                StackFixture.S3SecretKey
            ),
            new Amazon.S3.AmazonS3Config
            {
                ServiceURL = stack.S3Url,
                ForcePathStyle = true,
                AuthenticationRegion = "us-east-1",
            }
        );
        await using var dataSource = NpgsqlDataSource.Create(stack.ConnectionString);
        var s3 = new S3BlobStore(dataSource, TimeProvider.System, s3Client, StackFixture.Bucket);
        foreach (
            var (org, hash, inline) in await db.QueryAsync<(string, string, bool)>(
                "SELECT org_id, hash, data IS NOT NULL FROM casebox.blobs"
            )
        )
        {
            var blob = await (inline ? postgres : s3).GetAsync(org, hash, Ct);
            var text = Encoding.UTF8.GetString(blob!.Data).ToLowerInvariant();
            if (needles.Any(n => text.Contains(n, StringComparison.Ordinal)))
                found.Add($"blob {hash}");
        }

        return found;
    }
}
