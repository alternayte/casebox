using System.Xml.Linq;
using Dapper;
using Microsoft.AspNetCore.DataProtection.Repositories;
using Npgsql;

namespace Casebox.Server.Infrastructure;

// Keeps the Data Protection key ring in Postgres, so all server instances share it and a
// restart keeps sessions, CSRF tokens and integration secrets readable.
public sealed class PostgresKeyRepository(NpgsqlDataSource dataSource) : IXmlRepository
{
    public IReadOnlyCollection<XElement> GetAllElements()
    {
        using var connection = dataSource.OpenConnection();
        return connection.Query<string>("SELECT xml FROM casebox.data_protection_keys ORDER BY created_at")
            .Select(XElement.Parse)
            .ToList();
    }

    public void StoreElement(XElement element, string friendlyName)
    {
        using var connection = dataSource.OpenConnection();
        connection.Execute(
            "INSERT INTO casebox.data_protection_keys (id, xml) VALUES (@Id, @Xml) ON CONFLICT (id) DO NOTHING",
            new { Id = friendlyName, Xml = element.ToString(SaveOptions.DisableFormatting) });
    }
}
