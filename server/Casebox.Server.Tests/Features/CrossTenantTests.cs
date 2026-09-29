using System.Net;
using System.Net.Http.Json;
using System.Text;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.AspNetCore.Routing;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// SDD section 11: every API route is called as another tenant. Organisation B's admin and
// worker use organisation A's IDs and names; nothing of A may come back, and nothing of A may change.
public sealed class CrossTenantTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private sealed record Sample(string Method, string Path, object? Body, Caller Caller);

    private enum Caller { Admin, Worker, Ingest }

    [Fact]
    public void Every_API_route_has_a_cross_tenant_sample()
    {
        var samples = Samples(new Resources("ws", "tok", "job", new string('a', 64), "acct"))
            .Select(s => Route(s.Method, s.Path)).ToHashSet();
        var missing = Routes().Where(r => !samples.Contains(r)).ToList();
        Assert.True(missing.Count == 0, "Routes without a cross-tenant sample: " + string.Join(", ", missing));
    }

    [Fact]
    public async Task No_route_reads_or_changes_another_organisations_data()
    {
        var a = await SeedOrgAAsync();
        var before = await SnapshotAsync(a);
        var admin = await stack.ServerB.AdminAsync();
        var worker = await stack.ServerB.TokenClientAsync(TokenKind.Worker);
        var ingest = await stack.ServerB.TokenClientAsync(TokenKind.Ingest);

        foreach (var sample in Samples(a))
        {
            var client = sample.Caller switch { Caller.Admin => admin, Caller.Worker => worker, _ => ingest };
            using var request = new HttpRequestMessage(new HttpMethod(sample.Method), sample.Path);
            if (sample.Body is byte[] bytes) request.Content = new ByteArrayContent(bytes);
            else if (sample.Body is not null) request.Content = JsonContent.Create(sample.Body);
            using var response = await client.SendAsync(request, Ct);
            var body = await response.Content.ReadAsStringAsync(Ct);

            Assert.True((int)response.StatusCode < 500, $"{sample.Method} {sample.Path} answered {(int)response.StatusCode}: {body}");
            if (sample.Method == "GET")
            {
                foreach (var marker in a.Markers)
                    Assert.False(body.Contains(marker, StringComparison.Ordinal), $"{sample.Method} {sample.Path} returned organisation A's '{marker}'.");
            }
        }

        Assert.Equal(before, await SnapshotAsync(a));
    }

    private sealed record Resources(string Workspace, string TokenId, string JobId, string BlobHash, string AccountId, string SessionId = "claude-code:alpha-session")
    {
        public const string Repo = "github.com/alpha-secret/repo";
        public const string TokenName = "alpha-secret-token";

        public IEnumerable<string> Markers => [Workspace, Repo, TokenName, TokenId, JobId, BlobHash, AccountId];
    }

    private static IEnumerable<Sample> Samples(Resources a) =>
    [
        new("POST", "/api/v1/auth/local", new { password = "wrong" }, Caller.Admin),
        new("GET", "/api/v1/auth/methods", null, Caller.Admin),
        new("POST", "/api/v1/auth/device/code", new { }, Caller.Admin),
        new("POST", "/api/v1/auth/device/approve", new { userCode = "BCDF-GHJK" }, Caller.Admin),
        new("POST", "/api/v1/auth/device/token", new { deviceCode = "unknown" }, Caller.Admin),
        new("POST", "/api/v1/devices", null, Caller.Admin),
        new("GET", "/api/v1/auth/oidc/login", null, Caller.Admin),
        new("POST", "/api/v1/auth/logout", null, Caller.Worker),
        new("GET", "/api/v1/auth/csrf", null, Caller.Admin),
        new("GET", "/api/v1/me", null, Caller.Admin),
        new("GET", "/api/v1/accounts/", null, Caller.Admin),
        new("PUT", $"/api/v1/accounts/{a.AccountId}/role", new { role = "viewer" }, Caller.Admin),
        new("GET", "/api/v1/org/", null, Caller.Admin),
        new("PUT", "/api/v1/org/settings", new { promptMode = "full", k = 2, pseudonymPeriod = "month", budgets = new { monthlyUsd = 1, perEvaluationUsd = 1, confirmAboveUsd = 1 } }, Caller.Admin),
        new("GET", "/api/v1/workspaces/", null, Caller.Admin),
        new("GET", $"/api/v1/workspaces/{a.Workspace}", null, Caller.Admin),
        new("POST", $"/api/v1/workspaces/{a.Workspace}/repos", new { repo = "github.com/b/other" }, Caller.Admin),
        new("DELETE", $"/api/v1/workspaces/{a.Workspace}/repos/{Resources.Repo}", null, Caller.Admin),
        new("PUT", $"/api/v1/workspaces/{a.Workspace}/recipe", new { recipe = new { image = "b" } }, Caller.Admin),
        new("POST", $"/api/v1/workspaces/{a.Workspace}/recipe/validation", new { hash = "x", passed = true }, Caller.Admin),
        new("POST", $"/api/v1/workspaces/{a.Workspace}/recipe/confirmation", new { hash = "x" }, Caller.Admin),
        new("POST", "/api/v1/workspaces/", new { name = a.Workspace }, Caller.Admin),
        new("GET", "/api/v1/tokens/", null, Caller.Admin),
        new("POST", "/api/v1/tokens/", new { kind = "worker", name = "b token" }, Caller.Admin),
        new("DELETE", $"/api/v1/tokens/{a.TokenId}", null, Caller.Admin),
        new("POST", "/worker/v1/jobs/lease", new { workerId = "b", version = "t", kinds = new[] { IdleJobHandler.JobKind } }, Caller.Worker),
        new("POST", $"/worker/v1/jobs/{a.JobId}/heartbeat", new { workerId = "a-worker" }, Caller.Worker),
        new("POST", $"/worker/v1/jobs/{a.JobId}/complete", new { workerId = "a-worker", result = new { repo = "github.com/b/x" } }, Caller.Worker),
        new("POST", $"/worker/v1/jobs/{a.JobId}/fail", new { workerId = "a-worker", error = "x", retryable = false }, Caller.Worker),
        new("GET", $"/worker/v1/blobs/{a.BlobHash}", null, Caller.Worker),
        new("PUT", $"/worker/v1/blobs/{a.BlobHash}", Encoding.UTF8.GetBytes("not A's bytes"), Caller.Worker),
        new("GET", "/ingest/v1/config", null, Caller.Ingest),
        new("POST", "/ingest/v1/sessions", new
        {
            session = new { id = a.SessionId, agent = "claude-code", source = "import", person = "⟦cbx:email:b@example.com⟧", startedAt = DateTimeOffset.UtcNow },
            events = new[] { new { seq = 0, at = DateTimeOffset.UtcNow, kind = "prompt", text = "B writes into its own session" } },
        }, Caller.Ingest),
        new("POST", "/v1/logs", new { resourceLogs = Array.Empty<object>() }, Caller.Ingest),
        new("POST", "/v1/metrics", new { resourceMetrics = Array.Empty<object>() }, Caller.Ingest),
    ];

    private IEnumerable<string> Routes() =>
        stack.ServerA.Services.GetRequiredService<EndpointDataSource>().Endpoints
            .OfType<RouteEndpoint>()
            .Where(e => e.RoutePattern.RawText is { } p && new[] { "/api/v1", "/worker/v1", "/ingest/v1", "/v1/" }.Any(prefix => p.StartsWith(prefix, StringComparison.Ordinal)))
            .SelectMany(e => (e.Metadata.GetMetadata<Microsoft.AspNetCore.Routing.HttpMethodMetadata>()?.HttpMethods ?? ["ANY"]).Select(m => Route(m, e.RoutePattern.RawText!)))
            .Distinct();

    // Normalizes a pattern or a concrete path to "METHOD /segment/{}/…" so the two compare.
    private static string Route(string method, string path)
    {
        var segments = path.Split('/', StringSplitOptions.RemoveEmptyEntries);
        var known = new[] { "api", "v1", "worker", "auth", "local", "oidc", "login", "logout", "csrf", "me", "methods", "device", "code", "approve", "token", "devices", "accounts", "role", "org", "settings",
            "workspaces", "repos", "recipe", "validation", "confirmation", "tokens", "jobs", "lease", "heartbeat", "complete", "fail", "blobs",
            "ingest", "config", "sessions", "logs", "metrics" };
        var normalized = new List<string>();
        foreach (var s in segments)
        {
            if (known.Contains(s)) normalized.Add(s);
            else
            {
                normalized.Add("{}");
                if (s.StartsWith("{**", StringComparison.Ordinal) || normalized.Count >= 2 && normalized[^2] == "repos") break;
            }
        }

        return $"{method} /{string.Join('/', normalized)}";
    }

    private async Task<Resources> SeedOrgAAsync()
    {
        var admin = await stack.ServerA.AdminAsync();
        var workspace = $"alpha-secret-{Guid.NewGuid():N}"[..24];
        (await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)).EnsureSuccessStatusCode();
        (await admin.PostAsJsonAsync($"/api/v1/workspaces/{workspace}/repos", new { repo = Resources.Repo }, Ct)).EnsureSuccessStatusCode();
        var token = await (await admin.PostAsJsonAsync("/api/v1/tokens", new { kind = "worker", name = Resources.TokenName }, Ct))
            .Content.ReadFromJsonAsync<CaseboxServer.IssuedTokenBody>(Ct);
        var me = await admin.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);

        var data = Encoding.UTF8.GetBytes($"alpha secret blob {Guid.NewGuid()}");
        var hash = BlobStore.HashOf(data);
        await stack.ServerA.Services.GetRequiredService<BlobStore>().PutAsync(StackFixture.OrgA, hash, "text/plain", data, Ct);

        var queue = stack.ServerA.Services.GetRequiredService<JobQueue>();
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.OpenAsync(Ct);
        await using var transaction = await db.BeginTransactionAsync(Ct);
        var jobId = await queue.EnqueueAsync(transaction, StackFixture.OrgA, IdleJobHandler.JobKind, $"alpha:{Guid.NewGuid()}", new { workspace }, 3, Ct);
        await transaction.CommitAsync(Ct);

        return new Resources(workspace, token!.Info.Id, jobId, hash, me!.AccountId);
    }

    // Everything of organisation A that a cross-tenant call could change.
    private async Task<string> SnapshotAsync(Resources a)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var parts = new[]
        {
            await db.QuerySingleAsync<string>("SELECT repos::text || recipe_status FROM casebox.workspaces WHERE org_id = @Org AND name = @Name", new { Org = StackFixture.OrgA, Name = a.Workspace }),
            await db.QuerySingleAsync<string>("SELECT coalesce(revoked_at::text, 'active') FROM casebox.api_tokens WHERE id = @Id", new { Id = a.TokenId }),
            await db.QuerySingleAsync<string>("SELECT status || attempts FROM casebox.jobs WHERE id = @Id", new { Id = a.JobId }),
            await db.QuerySingleAsync<string>("SELECT role FROM casebox.accounts WHERE id = @Id", new { Id = a.AccountId }),
            await db.QuerySingleAsync<string>("SELECT count(*)::text FROM deedbox.events WHERE tenant_id = @Org AND stream_id = @Stream", new { Org = StackFixture.OrgA, Stream = $"workspace:{a.Workspace}" }),
            await db.QuerySingleAsync<string>("SELECT count(*)::text FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id", new { Org = StackFixture.OrgA, Id = a.SessionId }),
        };
        return string.Join('|', parts);
    }
}
