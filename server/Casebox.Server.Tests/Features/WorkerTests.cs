using System.Net;
using System.Net.Http.Json;
using System.Text;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Features.Workspaces;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

public sealed class WorkerTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task A_revoked_token_stops_working()
    {
        var admin = await stack.ServerA.AdminAsync();
        var issued = await (
            await admin.PostAsJsonAsync(
                "/api/v1/tokens",
                new { kind = "worker", name = "revoke me" },
                Ct
            )
        ).Content.ReadFromJsonAsync<CaseboxServer.IssuedTokenBody>(Ct);
        var worker = stack.ServerA.Anonymous();
        worker.DefaultRequestHeaders.Authorization = new("Bearer", issued!.Secret);
        Assert.NotEqual(
            HttpStatusCode.Unauthorized,
            (await Lease(worker, "w1", "none")).StatusCode
        );

        (await admin.DeleteAsync($"/api/v1/tokens/{issued.Info.Id}", Ct)).EnsureSuccessStatusCode();
        Assert.Equal(HttpStatusCode.Unauthorized, (await Lease(worker, "w1", "none")).StatusCode);
    }

    [Fact]
    public async Task An_ingest_token_cannot_lease_jobs()
    {
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        Assert.Equal(
            HttpStatusCode.Forbidden,
            (await Lease(ingest, "w1", TestJobHandler.JobKind)).StatusCode
        );
    }

    [Fact]
    public async Task A_job_result_appends_its_events_once_even_when_the_worker_posts_it_twice()
    {
        var workspace = await CreateWorkspaceAsync();
        var jobId = await EnqueueAsync(workspace);
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);

        var job = await (
            await Lease(worker, "w1", TestJobHandler.JobKind)
        ).Content.ReadFromJsonAsync<Job>(Ct);
        Assert.Equal(jobId, job!.Id);
        Assert.Equal(1, job.Attempts);

        var result = new { workerId = "w1", result = new { repo = "github.com/acme/api" } };
        Assert.Equal(
            HttpStatusCode.NoContent,
            (
                await worker.PostAsJsonAsync($"/worker/v1/jobs/{jobId}/complete", result, Ct)
            ).StatusCode
        );
        Assert.Equal(
            HttpStatusCode.NoContent,
            (
                await worker.PostAsJsonAsync($"/worker/v1/jobs/{jobId}/complete", result, Ct)
            ).StatusCode
        );

        Assert.Equal(
            1,
            await EventCountAsync(
                StackFixture.OrgA,
                Workspace.StreamIdFor(workspace),
                "workspace.repo_added"
            )
        );
        Assert.Equal("succeeded", await StatusAsync(jobId));
    }

    [Fact]
    public async Task Only_the_lease_holder_completes_a_job()
    {
        var jobId = await EnqueueAsync(await CreateWorkspaceAsync());
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        (await Lease(worker, "w1", TestJobHandler.JobKind)).EnsureSuccessStatusCode();

        var other = new { workerId = "w2", result = new { repo = "github.com/acme/api" } };
        Assert.Equal(
            HttpStatusCode.Conflict,
            (
                await worker.PostAsJsonAsync($"/worker/v1/jobs/{jobId}/complete", other, Ct)
            ).StatusCode
        );
        Assert.Equal(
            HttpStatusCode.Conflict,
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{jobId}/heartbeat",
                    new { workerId = "w2" },
                    Ct
                )
            ).StatusCode
        );
        Assert.Equal(
            HttpStatusCode.NoContent,
            (
                await worker.PostAsJsonAsync(
                    $"/worker/v1/jobs/{jobId}/heartbeat",
                    new { workerId = "w1" },
                    Ct
                )
            ).StatusCode
        );
    }

    [Fact]
    public async Task An_expired_lease_makes_the_job_available_again_until_its_attempts_run_out()
    {
        var jobId = await EnqueueAsync(await CreateWorkspaceAsync(), maxAttempts: 2);
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);

        (await Lease(worker, "w1", TestJobHandler.JobKind)).EnsureSuccessStatusCode();
        await ExpireLeaseAsync(jobId);
        var again = await (
            await Lease(worker, "w2", TestJobHandler.JobKind)
        ).Content.ReadFromJsonAsync<Job>(Ct);
        Assert.Equal((jobId, 2), (again!.Id, again.Attempts));

        await ExpireLeaseAsync(jobId);
        Assert.Equal(
            HttpStatusCode.NoContent,
            (await Lease(worker, "w3", TestJobHandler.JobKind)).StatusCode
        );
        Assert.Equal("failed", await StatusAsync(jobId));
    }

    [Fact]
    public async Task A_retryable_failure_waits_before_the_next_attempt()
    {
        var jobId = await EnqueueAsync(await CreateWorkspaceAsync());
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        (await Lease(worker, "w1", TestJobHandler.JobKind)).EnsureSuccessStatusCode();

        var fail = new
        {
            workerId = "w1",
            error = "sandbox start timed out",
            retryable = true,
        };
        Assert.Equal(
            HttpStatusCode.NoContent,
            (await worker.PostAsJsonAsync($"/worker/v1/jobs/{jobId}/fail", fail, Ct)).StatusCode
        );
        Assert.Equal("queued", await StatusAsync(jobId));
        Assert.Equal(
            HttpStatusCode.NoContent,
            (await Lease(worker, "w1", TestJobHandler.JobKind)).StatusCode
        );
    }

    [Fact]
    public async Task Concurrent_workers_never_lease_the_same_job()
    {
        var workspace = await CreateWorkspaceAsync();
        var jobs = new List<string>();
        for (var i = 0; i < 20; i++)
            jobs.Add(await EnqueueAsync(workspace));
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);

        var leases = await Task.WhenAll(
            Enumerable
                .Range(0, 30)
                .Select(async i =>
                {
                    var response = await Lease(worker, $"w{i}", TestJobHandler.JobKind);
                    return response.StatusCode == HttpStatusCode.OK
                        ? (await response.Content.ReadFromJsonAsync<Job>(Ct))!.Id
                        : null;
                })
        );

        var leased = leases.Where(id => id is not null && jobs.Contains(id)).ToList();
        Assert.Equal(leased.Count, leased.Distinct().Count());
    }

    private async Task<string> CreateWorkspaceAsync()
    {
        var admin = await stack.ServerA.AdminAsync();
        var name = $"jobs-{Guid.NewGuid():N}"[..20];
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name }, Ct)
        ).EnsureSuccessStatusCode();
        return name;
    }

    private async Task<string> EnqueueAsync(string workspace, int maxAttempts = 3)
    {
        var queue = stack.ServerA.Services.GetRequiredService<JobQueue>();
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.OpenAsync(Ct);
        await using var transaction = await db.BeginTransactionAsync(Ct);
        var id = await queue.EnqueueAsync(
            transaction,
            StackFixture.OrgA,
            TestJobHandler.JobKind,
            $"test:{Guid.NewGuid()}",
            new { workspace },
            maxAttempts,
            Ct
        );
        await transaction.CommitAsync(Ct);
        return id;
    }

    private static Task<HttpResponseMessage> Lease(
        HttpClient worker,
        string workerId,
        string kind
    ) =>
        worker.PostAsJsonAsync(
            "/worker/v1/jobs/lease",
            new
            {
                workerId,
                version = "test",
                kinds = new[] { kind },
            },
            Ct
        );

    private async Task ExpireLeaseAsync(string jobId)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.ExecuteAsync(
            "UPDATE casebox.jobs SET lease_expires_at = now() - interval '1 second' WHERE id = @Id",
            new { Id = jobId }
        );
    }

    private async Task<string> StatusAsync(string jobId)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        return await db.QuerySingleAsync<string>(
            "SELECT status FROM casebox.jobs WHERE id = @Id",
            new { Id = jobId }
        );
    }

    internal static async Task<int> EventCountAsync(
        string connectionString,
        string tenant,
        string streamId,
        string type
    )
    {
        await using var db = new NpgsqlConnection(connectionString);
        return await db.QuerySingleAsync<int>(
            """
            SELECT count(*) FROM deedbox.events e
            WHERE e.tenant_id = @Tenant AND e.stream_id = @Stream AND e.event_type = @Type
            """,
            new
            {
                Tenant = tenant,
                Stream = streamId,
                Type = type,
            }
        );
    }

    private Task<int> EventCountAsync(string tenant, string streamId, string type) =>
        EventCountAsync(stack.ConnectionString, tenant, streamId, type);
}
