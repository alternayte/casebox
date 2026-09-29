using System.Threading.RateLimiting;
using Amazon.Runtime;
using Amazon.S3;
using Azure.Identity;
using Casebox.Server;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Cases;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Health;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Jira;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Repos;
using Casebox.Server.Features.Steering;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Features.WorkItems;
using Casebox.Server.Features.Workspaces;
using Casebox.Server.Infrastructure;
using Casebox.Server.Infrastructure.Database;
using Dapper;
using Deedbox;
using Microsoft.AspNetCore.DataProtection;
using Microsoft.AspNetCore.Diagnostics.HealthChecks;
using Microsoft.Extensions.Diagnostics.HealthChecks;
using Microsoft.Extensions.Options;
using Npgsql;

var builder = WebApplication.CreateBuilder(args);
var options =
    builder.Configuration.GetSection(CaseboxOptions.Section).Get<CaseboxOptions>()
    ?? new CaseboxOptions();
builder.Services.Configure<CaseboxOptions>(
    builder.Configuration.GetSection(CaseboxOptions.Section)
);

// The management port serves /internal/effects and /healthz; it is added to the public URLs.
var urls = builder.Configuration["urls"] ?? "http://+:8080";
builder.WebHost.UseUrls($"{urls};http://+:{options.ManagementPort}");

DefaultTypeMap.MatchNamesWithUnderscores = true;
builder.Services.AddSingleton(TimeProvider.System);
builder.Services.AddSingleton(_ =>
    NpgsqlDataSource.Create(
        builder.Configuration.GetConnectionString("Casebox")
            ?? throw new InvalidOperationException(
                "Set ConnectionStrings__Casebox to the Postgres connection string."
            )
    )
);
builder.Services.AddSingleton<SchemaMigrator>();
builder.Services.AddSingleton<OrgBootstrap>();

builder.Services.ConfigureHttpJsonOptions(o =>
    o.SerializerOptions.Converters.Add(CaseboxStreams.Enums)
);

builder.Services.AddDeedbox(es =>
    CaseboxStreams.Register(
        es.UsePostgres(sp =>
                sp.GetRequiredService<IConfiguration>().GetConnectionString("Casebox")!
            )
            .ApplySchemaOnStartup()
            .Keys(keys => ConfigureKeys(keys, options.Keys))
    )
);

builder.Services.AddCaseboxAuth(options);
builder.Services.AddRateLimiter(o =>
{
    o.RejectionStatusCode = StatusCodes.Status429TooManyRequests;
    // A stolen ingest token can write only its organisation's sessions, and only this fast.
    o.AddPolicy(
        IngestRateLimit,
        http =>
            RateLimitPartition.GetTokenBucketLimiter(
                http.User.FindFirst(CaseboxClaims.Token)?.Value ?? "anonymous",
                _ => new TokenBucketRateLimiterOptions
                {
                    TokenLimit = 600,
                    TokensPerPeriod = 300,
                    ReplenishmentPeriod = TimeSpan.FromMinutes(1),
                }
            )
    );
    o.AddPolicy(
        AuthEndpoints.LoginRateLimit,
        http =>
            RateLimitPartition.GetFixedWindowLimiter(
                http.Connection.RemoteIpAddress?.ToString() ?? "unknown",
                _ => new FixedWindowRateLimiterOptions
                {
                    PermitLimit = 10,
                    Window = TimeSpan.FromMinutes(1),
                }
            )
    );
});

builder.Services.AddSingleton<JobQueue>();
builder.Services.AddMemoryCache();
builder.Services.AddDataProtection().SetApplicationName("casebox");
builder
    .Services.AddOptions<Microsoft.AspNetCore.DataProtection.KeyManagement.KeyManagementOptions>()
    .Configure<NpgsqlDataSource>((o, ds) => o.XmlRepository = new PostgresKeyRepository(ds));
builder.Services.AddSingleton<IntegrationStore>();
builder.Services.AddSingleton<GitHubClients>();
builder.Services.AddHttpClient("github");
builder.Services.AddHttpClient("jira");
builder.Services.AddHttpClient("queuebox");
builder.Services.AddScoped<Linker>();
builder.Services.AddSingleton<IJobResultHandler, EntireFetchResult>();
builder.Services.AddSingleton<IJobResultHandler, GitAiFetchResult>();
builder.Services.AddSingleton<IJobResultHandler, ClassifyResultHandler>();
builder.Services.AddSingleton<IJobResultHandler, PullRequestResultHandler>();
builder.Services.AddSingleton<IJobResultHandler, EnvBuild>();
builder.Services.AddSingleton<IJobResultHandler, MineResultHandler>();
builder.Services.AddSingleton<IJobResultHandler, ValidateResultHandler>();
builder.Services.AddSingleton<IJobResultHandler, InstructionResultHandler>();
builder.Services.AddScoped<CaseMining>();
builder.Services.AddScoped<SteeringScan>();
builder.Services.AddScoped<SteeringReports>();
builder.Services.AddScoped<SteeringWindows>();
builder.Services.AddSingleton<SteeringDetector>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<SteeringDetector>());
builder.Services.AddSingleton<RepoJobScheduler>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<RepoJobScheduler>());
builder.Services.AddScoped<GitHubReader>();
builder.Services.AddSingleton<GitHubPoller>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<GitHubPoller>());
builder.Services.AddSingleton<JiraPoller>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<JiraPoller>());
builder.Services.AddSingleton<IRosterSource, GitHubRosterSource>();
builder.Services.AddSingleton<IRosterSource, JiraRosterSource>();
builder.Services.AddScoped<IInboxHandler, PullRequestHandler>();
builder.Services.AddScoped<IInboxHandler, IssueHandler>();
builder.Services.AddScoped<IInboxHandler, RevertCommitHandler>();
builder.Services.AddScoped<IInboxHandler, JiraIssueHandler>();
foreach (
    var githubEvent in new[]
    {
        "pull_request",
        "pull_request_review",
        "pull_request_review_comment",
        "check_run",
        "workflow_run",
        "push",
        "issues",
    }
)
    builder.Services.AddScoped<IInboxHandler>(sp => new GitHubNoticeHandler(
        sp.GetRequiredService<GitHubPoller>()
    )
    {
        EventType = githubEvent,
    });
builder.Services.AddSingleton<Roster>();
builder.Services.AddScoped<Erasure>();
builder.Services.AddSingleton<Retention>();
builder.Services.AddHostedService(sp => sp.GetRequiredService<Retention>());
builder.Services.AddScoped<Identities>();
builder.Services.AddScoped<CaptureStore>();
builder.Services.AddSingleton<BlobStore>(sp =>
    options.Blobs.Store switch
    {
        "postgres" => new PostgresBlobStore(
            sp.GetRequiredService<NpgsqlDataSource>(),
            sp.GetRequiredService<TimeProvider>()
        ),
        "s3" => new S3BlobStore(
            sp.GetRequiredService<NpgsqlDataSource>(),
            sp.GetRequiredService<TimeProvider>(),
            S3Client(options.Blobs),
            options.Blobs.S3Bucket
                ?? throw new InvalidOperationException(
                    "Set Casebox__Blobs__S3Bucket for the s3 blob store."
                )
        ),
        var other => throw new InvalidOperationException(
            $"Casebox__Blobs__Store is '{other}'; use postgres or s3."
        ),
    }
);

builder.Services.AddHttpClient<PollPublisher>();

// One consumer per QueueBox source. AddHostedService would keep only the first registration of a
// type, so each consumer is registered as its own IHostedService.
foreach (var source in new[] { InboxSources.Poll, GitHubWebhooks.Source })
    builder.Services.AddSingleton<IHostedService>(sp =>
        ActivatorUtilities.CreateInstance<InboxConsumer>(sp, source)
    );

builder.Services.AddHttpClient<QueueBoxHealthCheck>();
builder
    .Services.AddHealthChecks()
    .AddCheck<PostgresHealthCheck>("postgres", tags: ["ready"])
    .AddCheck<QueueBoxHealthCheck>("queuebox", tags: ["ready"])
    .AddCheck<WorkersHealthCheck>("workers", tags: ["ready"])
    .AddDeedboxHealthChecks();

builder.Services.AddOpenApi();
builder.Services.AddProblemDetails();
builder.Services.AddExceptionHandler<ProblemExceptionHandler>();

var app = builder.Build();

if (options.Keys.Mode == "database")
    app.Logger.LogWarning(
        "Deedbox keys are in database mode: the master key sits next to the data it protects. Use environment or azure mode in production."
    );

await app.Services.GetRequiredService<SchemaMigrator>().MigrateAsync(CancellationToken.None);

app.UseExceptionHandler();
app.UseDefaultFiles();
app.UseStaticFiles();
app.UseAuthentication();
app.UseMiddleware<TenancyMiddleware>();
app.UseRateLimiter();
app.UseAuthorization();

var management = $"*:{options.ManagementPort}";
var internalRoutes = app.MapGroup("").RequireHost(management);
internalRoutes.MapEffects();
internalRoutes.MapHealthChecks("/healthz/live", new HealthCheckOptions { Predicate = _ => false });
internalRoutes.MapHealthChecks(
    "/healthz/ready",
    new HealthCheckOptions
    {
        Predicate = c =>
            c.Tags.Contains("ready") || c.Name.StartsWith("deedbox", StringComparison.Ordinal),
    }
);

var api = app.MapGroup("/api/v1").RequireCsrf();
api.MapAuth();
api.MapDeviceLogin();
api.MapOrg();
api.MapWorkspaces();
api.MapTokens();
api.MapPrivacy();
api.MapIntegrations();
api.MapWorkItems();
api.MapSteering();
api.MapCases();

app.MapGroup("").RequireRateLimiting(IngestRateLimit).MapIngest();
app.MapGroup("").RequireRateLimiting(IngestRateLimit).MapOtlp();
app.MapGitHubWebhooks();

var worker = app.MapGroup("/worker/v1");
worker.MapWorkerJobs();
worker.MapWorkerBlobs();
worker.MapRepoJobs();
worker.MapSteeringWorker();
worker.MapCaseWorker();

app.MapOpenApi();
app.MapFallbackToFile("index.html");

// Deedbox applies its schema and the org exists before the first request.
app.Lifetime.ApplicationStarted.Register(() =>
    app.Services.GetRequiredService<OrgBootstrap>().EnsureAsync().GetAwaiter().GetResult()
);

app.Run();

static void ConfigureKeys(KeysBuilder keys, CaseboxOptions.KeysOptions options)
{
    switch (options.Mode)
    {
        case "database":
            keys.StoreInDatabase();
            break;
        case "environment":
            keys.FromEnvironment(options.Variable);
            break;
        case "azure":
            keys.UseAzureKeyVault(
                new Uri(
                    options.AzureKeyId
                        ?? throw new InvalidOperationException(
                            "Set Casebox__Keys__AzureKeyId for azure key mode."
                        )
                ),
                new DefaultAzureCredential()
            );
            break;
        default:
            throw new InvalidOperationException(
                "Set Casebox__Keys__Mode to database, environment or azure. `casebox up` uses database."
            );
    }
}

static AmazonS3Client S3Client(CaseboxOptions.BlobOptions blobs)
{
    var config = new AmazonS3Config { ForcePathStyle = true };
    if (blobs.S3ServiceUrl is { } url)
        config.ServiceURL = url;
    if (blobs.S3Region is { } region)
        config.AuthenticationRegion = region;
    return blobs.S3AccessKey is { } key
        ? new AmazonS3Client(new BasicAWSCredentials(key, blobs.S3SecretKey), config)
        : new AmazonS3Client(config);
}

public partial class Program
{
    private const string IngestRateLimit = "ingest";
}
