using DotNet.Testcontainers.Builders;
using DotNet.Testcontainers.Containers;
using DotNet.Testcontainers.Networks;
using Testcontainers.PostgreSql;

namespace Casebox.Server.Tests.Infrastructure;

// One Postgres 16 and one QueueBox on a private network, shared by every test in the assembly.
// QueueBox migrates its own outbox and inbox tables into the same database on start.
public sealed class DatabaseFixture : IAsyncLifetime
{
    private const string Database = "casebox";
    private const string User = "casebox";
    private const string Password = "casebox";

    private readonly INetwork _network = new NetworkBuilder().Build();
    private PostgreSqlContainer? _postgres;
    private IContainer? _queueBox;

    public string ConnectionString =>
        _postgres?.GetConnectionString() ?? throw new InvalidOperationException("The fixture has not started.");

    public Uri QueueBoxUrl =>
        _queueBox is null
            ? throw new InvalidOperationException("The fixture has not started.")
            : new Uri($"http://{_queueBox.Hostname}:{_queueBox.GetMappedPublicPort(8080)}");

    public async ValueTask InitializeAsync()
    {
        await _network.CreateAsync();

        _postgres = new PostgreSqlBuilder(Images.Postgres)
            .WithDatabase(Database)
            .WithUsername(User)
            .WithPassword(Password)
            .WithNetwork(_network)
            .WithNetworkAliases("postgres")
            .Build();
        await _postgres.StartAsync();

        _queueBox = new ContainerBuilder(Images.QueueBox)
            .WithNetwork(_network)
            .WithEnvironment("QUEUEBOX_DATABASE_URL", $"jdbc:postgresql://postgres:5432/{Database}")
            .WithEnvironment("QUEUEBOX_DATABASE_USERNAME", User)
            .WithEnvironment("QUEUEBOX_DATABASE_PASSWORD", Password)
            .WithPortBinding(8080, true)
            .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(r => r.ForPort(8080).ForPath("/health")))
            .Build();
        await _queueBox.StartAsync();
    }

    public async ValueTask DisposeAsync()
    {
        if (_queueBox is not null) await _queueBox.DisposeAsync();
        if (_postgres is not null) await _postgres.DisposeAsync();
        await _network.DisposeAsync();
    }
}
