using System.Reflection;
using Dapper;
using Npgsql;

namespace Casebox.Server.Infrastructure.Database;

// Applies the embedded, numbered migrations in order, once each, under an advisory lock, so
// several server instances can start at the same time. Migrations only move forward.
internal sealed class SchemaMigrator(NpgsqlDataSource dataSource, ILogger<SchemaMigrator> logger)
{
    private const long LockKey = 0x43415345424F58; // "CASEBOX"

    public async Task MigrateAsync(CancellationToken cancellationToken)
    {
        await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);
        await connection.ExecuteAsync(
            new CommandDefinition(
                "SELECT pg_advisory_lock(@LockKey)",
                new { LockKey },
                cancellationToken: cancellationToken
            )
        );
        try
        {
            await connection.ExecuteAsync(
                new CommandDefinition(
                    """
                    CREATE SCHEMA IF NOT EXISTS casebox;
                    CREATE TABLE IF NOT EXISTS casebox.schema_migrations (
                        version    integer     PRIMARY KEY,
                        name       text        NOT NULL,
                        applied_at timestamptz NOT NULL DEFAULT now()
                    );
                    """,
                    cancellationToken: cancellationToken
                )
            );

            var applied = (
                await connection.QueryAsync<int>(
                    new CommandDefinition(
                        "SELECT version FROM casebox.schema_migrations",
                        cancellationToken: cancellationToken
                    )
                )
            ).ToHashSet();

            foreach (var migration in Migrations().Where(m => !applied.Contains(m.Version)))
            {
                await using var transaction = await connection.BeginTransactionAsync(
                    cancellationToken
                );
                await connection.ExecuteAsync(
                    new CommandDefinition(
                        migration.Sql,
                        transaction: transaction,
                        cancellationToken: cancellationToken
                    )
                );
                await connection.ExecuteAsync(
                    new CommandDefinition(
                        "INSERT INTO casebox.schema_migrations (version, name) VALUES (@Version, @Name)",
                        new { migration.Version, migration.Name },
                        transaction,
                        cancellationToken: cancellationToken
                    )
                );
                await transaction.CommitAsync(cancellationToken);
                logger.LogInformation(
                    "Applied migration {Version} {Name}",
                    migration.Version,
                    migration.Name
                );
            }
        }
        finally
        {
            await connection.ExecuteAsync(
                new CommandDefinition(
                    "SELECT pg_advisory_unlock(@LockKey)",
                    new { LockKey },
                    cancellationToken: CancellationToken.None
                )
            );
        }
    }

    internal static IReadOnlyList<Migration> Migrations()
    {
        var assembly = typeof(SchemaMigrator).Assembly;
        const string prefix = "Casebox.Server.Infrastructure.Database.Migrations.";
        var migrations = assembly
            .GetManifestResourceNames()
            .Where(n =>
                n.StartsWith(prefix, StringComparison.Ordinal)
                && n.EndsWith(".sql", StringComparison.Ordinal)
            )
            .Select(n =>
            {
                var file = n[prefix.Length..^".sql".Length];
                var separator = file.IndexOf('_', StringComparison.Ordinal);
                var version = int.Parse(
                    file[..separator],
                    System.Globalization.CultureInfo.InvariantCulture
                );
                using var stream = assembly.GetManifestResourceStream(n)!;
                using var reader = new StreamReader(stream);
                return new Migration(version, file[(separator + 1)..], reader.ReadToEnd());
            })
            .OrderBy(m => m.Version)
            .ToList();

        for (var i = 0; i < migrations.Count; i++)
        {
            if (migrations[i].Version != i + 1)
                throw new InvalidOperationException(
                    $"Migration numbers must run 1, 2, 3 without gaps; found {migrations[i].Version} at position {i + 1}."
                );
        }

        return migrations;
    }

    internal sealed record Migration(int Version, string Name, string Sql);
}
