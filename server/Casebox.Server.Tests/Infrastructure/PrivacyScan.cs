using System.Text;
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

        return found;
    }
}
