using System.Net;
using System.Net.Sockets;
using DotNet.Testcontainers.Builders;
using DotNet.Testcontainers.Configurations;
using DotNet.Testcontainers.Containers;
using DotNet.Testcontainers.Networks;
using Testcontainers.PostgreSql;

namespace Casebox.Server.Tests.Infrastructure;

// The whole stack, shared by every test in the assembly: Postgres 16, QueueBox with the real
// deploy/queuebox.yml, a mock OIDC provider, an S3-compatible store, and two Casebox servers for
// two organisations on one database, as in hosted mode.
public sealed class StackFixture : IAsyncLifetime
{
    public const string OrgA = "org-a";
    public const string OrgB = "org-b";
    public const string EffectsToken = "test-effects-token";
    public const string PollToken = "test-poll-token";
    public const string AdminPassword = "test-admin-password";
    public const string Bucket = "casebox-test";
    public const string S3AccessKey = "casebox";
    public const string S3SecretKey = "casebox-secret";

    private const string Database = "casebox";
    private const string User = "casebox";
    private const string Password = "casebox";

    private readonly INetwork _network = new NetworkBuilder().Build();
    private PostgreSqlContainer? _postgres;
    private IContainer? _queueBox;
    private IContainer? _oidc;
    private IContainer? _s3;

    public string ConnectionString => _postgres!.GetConnectionString();

    public Uri QueueBoxUrl => new($"http://{_queueBox!.Hostname}:{_queueBox.GetMappedPublicPort(8080)}");

    public Uri QueueBoxHealthUrl => new($"http://{_queueBox!.Hostname}:{_queueBox.GetMappedPublicPort(9090)}/health");

    public string OidcAuthority => $"http://localhost:{_oidc!.GetMappedPublicPort(8080)}/default";

    public string S3Url => $"http://localhost:{_s3!.GetMappedPublicPort(9000)}";

    public FakeServices Fakes { get; } = new();

    public CaseboxServer ServerA { get; private set; } = null!;

    public CaseboxServer ServerB { get; private set; } = null!;

    public async ValueTask InitializeAsync()
    {
        var (apiA, managementA, apiB, managementB) = (FreePort(), FreePort(), FreePort(), FreePort());
        // QueueBox delivers effects to server A's management port on the host.
        await TestcontainersSettings.ExposeHostPortsAsync((ushort)managementA);
        await _network.CreateAsync();

        _postgres = new PostgreSqlBuilder(Images.Postgres)
            .WithDatabase(Database).WithUsername(User).WithPassword(Password)
            .WithNetwork(_network).WithNetworkAliases("postgres")
            .Build();

        _oidc = new ContainerBuilder(Images.Oidc)
            .WithEnvironment("JSON_CONFIG", """{"interactiveLogin": false}""")
            .WithPortBinding(8080, true)
            .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(r => r.ForPort(8080).ForPath("/default/.well-known/openid-configuration")))
            .Build();

        _s3 = new ContainerBuilder(Images.S3)
            .WithEnvironment("RUSTFS_ACCESS_KEY", S3AccessKey)
            .WithEnvironment("RUSTFS_SECRET_KEY", S3SecretKey)
            .WithPortBinding(9000, true)
            .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(r => r.ForPort(9000).ForPath("/health")))
            .Build();

        await Task.WhenAll(_postgres.StartAsync(), _oidc.StartAsync(), _s3.StartAsync());

        _queueBox = new ContainerBuilder(Images.QueueBox)
            .WithNetwork(_network)
            .WithResourceMapping(new FileInfo(Path.Combine(RepoRoot(), "deploy", "queuebox.yml")), "/etc/queuebox/")
            .WithEnvironment("QUEUEBOX_CONFIG_FILE", "/etc/queuebox/queuebox.yml")
            .WithEnvironment("QUEUEBOX_DATABASE_URL", $"jdbc:postgresql://postgres:5432/{Database}")
            .WithEnvironment("QUEUEBOX_DATABASE_USERNAME", User)
            .WithEnvironment("QUEUEBOX_DATABASE_PASSWORD", Password)
            .WithEnvironment("CASEBOX_QUEUEBOX_ADMIN_TOKEN", "test-admin-token")
            .WithEnvironment("CASEBOX_EFFECTS_URL", $"http://host.testcontainers.internal:{managementA}")
            .WithEnvironment("CASEBOX_EFFECTS_TOKEN", EffectsToken)
            .WithEnvironment("CASEBOX_POLL_TOKEN", PollToken)
            // Fast retries keep the retry tests short; production uses the file's backoff.
            .WithEnvironment("QUEUEBOX_OUTBOX_RETRYBASEDELAYMS", "100")
            .WithPortBinding(8080, true)
            .WithPortBinding(9090, true)
            .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(r => r.ForPort(9090).ForPath("/health")))
            .Build();
        await _queueBox.StartAsync();

        ServerA = new CaseboxServer(this, OrgA, apiA, managementA);
        ServerB = new CaseboxServer(this, OrgB, apiB, managementB);
        ServerA.Start();
        ServerB.Start();
    }

    public async ValueTask DisposeAsync()
    {
        await ServerA.DisposeAsync();
        await ServerB.DisposeAsync();
        foreach (var container in new[] { _queueBox, _oidc, _s3, _postgres })
            if (container is not null) await container.DisposeAsync();
        await _network.DisposeAsync();
        await Fakes.DisposeAsync();
    }

    public static string RepoRoot()
    {
        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir is not null; dir = dir.Parent)
            if (File.Exists(Path.Combine(dir.FullName, "justfile"))) return dir.FullName;
        throw new InvalidOperationException("The repository root (the directory with the justfile) was not found.");
    }

    private static int FreePort()
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        return ((IPEndPoint)listener.LocalEndpoint).Port;
    }
}
