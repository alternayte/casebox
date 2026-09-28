using Npgsql;

namespace Casebox.Server.Tests.Infrastructure;

// Pins the QueueBox image: QueueBox.Inbox needs the V10 schema, which adds inbox.headers.
public sealed class QueueBoxSchemaTests(DatabaseFixture db)
{
    [Fact]
    public async Task Pinned_image_migrates_the_outbox_and_the_inbox_that_QueueBox_Inbox_needs()
    {
        await using var connection = new NpgsqlConnection(db.ConnectionString);
        await connection.OpenAsync(TestContext.Current.CancellationToken);
        await using var command = new NpgsqlCommand(
            """
            SELECT table_name, column_name FROM information_schema.columns
            WHERE table_schema = 'public' AND table_name IN ('outbox', 'inbox')
            """,
            connection);
        var columns = new HashSet<string>();
        await using var reader = await command.ExecuteReaderAsync(TestContext.Current.CancellationToken);
        while (await reader.ReadAsync(TestContext.Current.CancellationToken))
            columns.Add($"{reader.GetString(0)}.{reader.GetString(1)}");

        Assert.Contains("outbox.id", columns);
        Assert.Contains("inbox.headers", columns);
    }
}
