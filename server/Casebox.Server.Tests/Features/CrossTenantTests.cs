using System.Net;
using System.Net.Http.Json;
using System.Text;
using Casebox.Server.Features.Auth;
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

    private enum Caller
    {
        Admin,
        Worker,
        Ingest,
    }

    [Fact]
    public void Every_API_route_has_a_cross_tenant_sample()
    {
        var samples = Samples(new Resources("ws", "tok", "job", new string('a', 64), "acct"))
            .Select(s => Route(s.Method, s.Path))
            .ToHashSet();
        var missing = Routes().Where(r => !samples.Contains(r)).ToList();
        Assert.True(
            missing.Count == 0,
            "Routes without a cross-tenant sample: " + string.Join(", ", missing)
        );
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
            var client = sample.Caller switch
            {
                Caller.Admin => admin,
                Caller.Worker => worker,
                _ => ingest,
            };
            using var request = new HttpRequestMessage(new HttpMethod(sample.Method), sample.Path);
            if (sample.Body is byte[] bytes)
                request.Content = new ByteArrayContent(bytes);
            else if (sample.Body is not null)
                request.Content = JsonContent.Create(sample.Body);
            using var response = await client.SendAsync(request, Ct);
            var body = await response.Content.ReadAsStringAsync(Ct);

            Assert.True(
                (int)response.StatusCode < 500,
                $"{sample.Method} {sample.Path} answered {(int)response.StatusCode}: {body}"
            );
            if (sample.Method == "GET")
            {
                foreach (var marker in a.Markers)
                    Assert.False(
                        body.Contains(marker, StringComparison.Ordinal),
                        $"{sample.Method} {sample.Path} returned organisation A's '{marker}'."
                    );
            }
        }

        Assert.Equal(before, await SnapshotAsync(a));
    }

    private sealed record Resources(
        string Workspace,
        string TokenId,
        string JobId,
        string AccountId,
        string SessionId = "claude-code:alpha-session"
    )
    {
        public const string Repo = "github.com/alpha-secret/repo";
        public const string TokenName = "alpha-secret-token";

        public IEnumerable<string> Markers =>
            [Workspace, Repo, TokenName, TokenId, JobId, AccountId];
    }

    private static IEnumerable<Sample> Samples(Resources a) =>
        [
            new("POST", "/api/v1/auth/local", new { password = "wrong" }, Caller.Admin),
            new("GET", "/api/v1/auth/methods", null, Caller.Admin),
            new("POST", "/api/v1/auth/device/code", new { }, Caller.Admin),
            new(
                "POST",
                "/api/v1/auth/device/approve",
                new { userCode = "BCDF-GHJK" },
                Caller.Admin
            ),
            new("POST", "/api/v1/auth/device/token", new { deviceCode = "unknown" }, Caller.Admin),
            new("POST", "/api/v1/devices", null, Caller.Admin),
            new("GET", "/api/v1/auth/oidc/login", null, Caller.Admin),
            new("POST", "/api/v1/auth/logout", null, Caller.Worker),
            new("GET", "/api/v1/auth/csrf", null, Caller.Admin),
            new("GET", "/api/v1/me", null, Caller.Admin),
            new("GET", "/api/v1/accounts/", null, Caller.Admin),
            new(
                "PUT",
                $"/api/v1/accounts/{a.AccountId}/role",
                new { role = "viewer" },
                Caller.Admin
            ),
            new("GET", "/api/v1/org/", null, Caller.Admin),
            new(
                "PUT",
                "/api/v1/org/settings",
                new
                {
                    promptMode = "full",
                    k = 2,
                    pseudonymPeriod = "month",
                },
                Caller.Admin
            ),
            new("GET", "/api/v1/workspaces/", null, Caller.Admin),
            new("GET", $"/api/v1/workspaces/{a.Workspace}", null, Caller.Admin),
            new(
                "POST",
                $"/api/v1/workspaces/{a.Workspace}/repos",
                new { repo = "github.com/b/other" },
                Caller.Admin
            ),
            new(
                "DELETE",
                $"/api/v1/workspaces/{a.Workspace}/repos/{Resources.Repo}",
                null,
                Caller.Admin
            ),
            new("POST", "/api/v1/workspaces/", new { name = a.Workspace }, Caller.Admin),
            new("GET", "/api/v1/tokens/", null, Caller.Admin),
            new("POST", "/api/v1/tokens/", new { kind = "worker", name = "b token" }, Caller.Admin),
            new("DELETE", $"/api/v1/tokens/{a.TokenId}", null, Caller.Admin),
            new(
                "POST",
                "/worker/v1/jobs/lease",
                new
                {
                    workerId = "b",
                    version = "t",
                    kinds = new[] { IdleJobHandler.JobKind },
                },
                Caller.Worker
            ),
            new(
                "POST",
                $"/worker/v1/jobs/{a.JobId}/heartbeat",
                new { workerId = "a-worker" },
                Caller.Worker
            ),
            new(
                "POST",
                $"/worker/v1/jobs/{a.JobId}/complete",
                new { workerId = "a-worker", result = new { repo = "github.com/b/x" } },
                Caller.Worker
            ),
            new(
                "POST",
                $"/worker/v1/jobs/{a.JobId}/fail",
                new
                {
                    workerId = "a-worker",
                    error = "x",
                    retryable = false,
                },
                Caller.Worker
            ),
            new(
                "POST",
                "/worker/v1/sessions",
                new
                {
                    session = new
                    {
                        id = a.SessionId,
                        agent = "claude-code",
                        source = "entire",
                        person = "⟦cbx:email:b@example.com⟧",
                        startedAt = DateTimeOffset.UtcNow,
                    },
                    events = new[]
                    {
                        new
                        {
                            seq = 3_000_000_000,
                            at = DateTimeOffset.UtcNow,
                            kind = "prompt",
                            text = "B's own session",
                        },
                    },
                },
                Caller.Worker
            ),
            new(
                "POST",
                "/worker/v1/attributions",
                new { repo = "github.com/alpha-secret/repo", commits = Array.Empty<object>() },
                Caller.Worker
            ),
            new("GET", "/ingest/v1/config", null, Caller.Ingest),
            new(
                "POST",
                "/ingest/v1/sessions",
                new
                {
                    session = new
                    {
                        id = a.SessionId,
                        agent = "claude-code",
                        source = "import",
                        person = "⟦cbx:email:b@example.com⟧",
                        startedAt = DateTimeOffset.UtcNow,
                    },
                    events = new[]
                    {
                        new
                        {
                            seq = 0,
                            at = DateTimeOffset.UtcNow,
                            kind = "prompt",
                            text = "B writes into its own session",
                        },
                    },
                },
                Caller.Ingest
            ),
            new("GET", "/api/v1/integrations/", null, Caller.Admin),
            new(
                "PUT",
                "/api/v1/integrations/github",
                new { mode = "token", token = "" },
                Caller.Admin
            ),
            new(
                "PUT",
                "/api/v1/integrations/jira",
                new
                {
                    url = "not a url",
                    token = "",
                    projects = Array.Empty<string>(),
                },
                Caller.Admin
            ),
            new(
                "PUT",
                "/api/v1/integrations/azure-devops",
                new { url = "not a url", token = "" },
                Caller.Admin
            ),
            new("GET", $"/api/v1/repos/host?repo={Resources.Repo}", null, Caller.Admin),
            new("GET", "/worker/v1/repos", null, Caller.Worker),
            new("DELETE", "/api/v1/integrations/unknown", null, Caller.Admin),
            new("GET", "/api/v1/work-items/", null, Caller.Admin),
            new("GET", "/api/v1/work-items/timeline?id=wi:jira:PAY-1", null, Caller.Admin),
            new(
                "PUT",
                $"/api/v1/sessions/{a.SessionId}/work-item",
                new { workItem = "wi:jira:PAY-1" },
                Caller.Admin
            ),
            new(
                "POST",
                "/api/v1/privacy/erasures",
                new { identity = "email:alpha-owner@example.com" },
                Caller.Admin
            ),
            new(
                "GET",
                $"/api/v1/steering/report?repo={Resources.Repo}&groupBy=agent",
                null,
                Caller.Admin
            ),
            new(
                "GET",
                $"/api/v1/steering/interventions?wentWrong=broke_convention&repo={Resources.Repo}",
                null,
                Caller.Admin
            ),
            new(
                "POST",
                "/api/v1/steering/relabel",
                new { @ref = "000000000000000000000000", intent = "direction" },
                Caller.Admin
            ),
            new("GET", $"/api/v1/steering/status?repo={Resources.Repo}", null, Caller.Admin),
            new("POST", "/api/v1/steering/refresh", new { repo = Resources.Repo }, Caller.Admin),
            new("GET", "/api/v1/steering/agreement", null, Caller.Admin),
            new(
                "POST",
                "/worker/v1/steering/windows",
                new
                {
                    stream = $"steering:session:{a.SessionId}",
                    interventionIds = new[] { "e:1" },
                    taskFor = a.SessionId,
                },
                Caller.Worker
            ),
            new("GET", "/worker/v1/steering/examples", null, Caller.Worker),
            new("GET", "/api/v1/patterns/?workspace=" + a.Workspace, null, Caller.Admin),
            new("GET", "/api/v1/patterns/00000000000000000000", null, Caller.Admin),
            new(
                "POST",
                "/api/v1/patterns/00000000000000000000/acknowledgement",
                null,
                Caller.Admin
            ),
            new(
                "POST",
                "/api/v1/patterns/00000000000000000000/dismissal",
                new { reason = "b" },
                Caller.Admin
            ),
            new("GET", "/api/v1/proposals/?workspace=" + a.Workspace, null, Caller.Admin),
            new("GET", "/api/v1/proposals/01J0000000000000000000000A", null, Caller.Admin),
            new(
                "POST",
                "/api/v1/proposals/01J0000000000000000000000A/rejection",
                new { reason = "b" },
                Caller.Admin
            ),
            new("GET", "/worker/v1/patterns/00000000000000000000/evidence", null, Caller.Worker),
            new("POST", "/v1/logs", new { resourceLogs = Array.Empty<object>() }, Caller.Ingest),
            new(
                "POST",
                "/v1/metrics",
                new { resourceMetrics = Array.Empty<object>() },
                Caller.Ingest
            ),
            new(
                "POST",
                "/api/v1/proposals/01J0000000000000000000000A/approval",
                null,
                Caller.Admin
            ),
            new(
                "POST",
                "/api/v1/proposals/01J0000000000000000000000A/applied",
                new { mode = "private" },
                Caller.Admin
            ),
        ];

    private IEnumerable<string> Routes() =>
        stack
            .ServerA.Services.GetRequiredService<EndpointDataSource>()
            .Endpoints.OfType<RouteEndpoint>()
            .Where(e =>
                e.RoutePattern.RawText is { } p
                && new[] { "/api/v1", "/worker/v1", "/ingest/v1", "/v1/" }.Any(prefix =>
                    p.StartsWith(prefix, StringComparison.Ordinal)
                )
            )
            .SelectMany(e =>
                (
                    e.Metadata.GetMetadata<Microsoft.AspNetCore.Routing.HttpMethodMetadata>()?.HttpMethods
                    ?? ["ANY"]
                ).Select(m => Route(m, e.RoutePattern.RawText!))
            )
            .Distinct();

    // Normalizes a pattern or a concrete path to "METHOD /segment/{}/…" so the two compare.
    private static string Route(string method, string path)
    {
        var segments = path.Split('?')[0].Split('/', StringSplitOptions.RemoveEmptyEntries);
        var known = new[]
        {
            "api",
            "v1",
            "worker",
            "auth",
            "local",
            "oidc",
            "login",
            "logout",
            "csrf",
            "me",
            "methods",
            "device",
            "code",
            "approve",
            "token",
            "devices",
            "accounts",
            "role",
            "org",
            "settings",
            "workspaces",
            "repos",
            "recipe",
            "validation",
            "confirmation",
            "tokens",
            "jobs",
            "lease",
            "heartbeat",
            "complete",
            "fail",
            "blobs",
            "ingest",
            "config",
            "sessions",
            "logs",
            "metrics",
            "privacy",
            "erasures",
            "integrations",
            "github",
            "jira",
            "azure-devops",
            "host",
            "work-items",
            "timeline",
            "work-item",
            "attributions",
            "steering",
            "report",
            "interventions",
            "relabel",
            "status",
            "refresh",
            "agreement",
            "windows",
            "examples",
            "cases",
            "queue",
            "instruction",
            "assertions",
            "approval",
            "approvals",
            "rejection",
            "retirement",
            "mining",
            "source",
            "validations",
            "oracle",
            "evaluations",
            "estimate",
            "confirmation",
            "cancellation",
            "runs",
            "offer",
            "harness",
            "ci",
            "baselines",
            "pull-requests",
            "patterns",
            "acknowledgement",
            "dismissal",
            "proposer",
            "proposals",
            "rejection",
            "evidence",
            "applied",
        };
        var normalized = new List<string>();
        foreach (var s in segments)
        {
            if (known.Contains(s))
                normalized.Add(s);
            else
            {
                normalized.Add("{}");
                if (
                    s.StartsWith("{**", StringComparison.Ordinal)
                    || normalized.Count >= 2 && normalized[^2] == "repos"
                )
                    break;
            }
        }

        return $"{method} /{string.Join('/', normalized)}";
    }

    private async Task<Resources> SeedOrgAAsync()
    {
        var admin = await stack.ServerA.AdminAsync();
        var workspace = $"alpha-secret-{Guid.NewGuid():N}"[..24];
        (
            await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = workspace }, Ct)
        ).EnsureSuccessStatusCode();
        (
            await admin.PostAsJsonAsync(
                $"/api/v1/workspaces/{workspace}/repos",
                new { repo = Resources.Repo },
                Ct
            )
        ).EnsureSuccessStatusCode();
        var token = await (
            await admin.PostAsJsonAsync(
                "/api/v1/tokens",
                new { kind = "worker", name = Resources.TokenName },
                Ct
            )
        ).Content.ReadFromJsonAsync<CaseboxServer.IssuedTokenBody>(Ct);
        var me = await admin.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);

        var queue = stack.ServerA.Services.GetRequiredService<JobQueue>();
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.OpenAsync(Ct);
        await using var transaction = await db.BeginTransactionAsync(Ct);
        var jobId = await queue.EnqueueAsync(
            transaction,
            StackFixture.OrgA,
            IdleJobHandler.JobKind,
            $"alpha:{Guid.NewGuid()}",
            new { workspace },
            3,
            Ct
        );
        await transaction.CommitAsync(Ct);

        return new Resources(workspace, token!.Info.Id, jobId, me!.AccountId);
    }

    // Everything of organisation A that a cross-tenant call could change.
    private async Task<string> SnapshotAsync(Resources a)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var parts = new[]
        {
            await db.QuerySingleAsync<string>(
                "SELECT repos::text FROM casebox.workspaces WHERE org_id = @Org AND name = @Name",
                new { Org = StackFixture.OrgA, Name = a.Workspace }
            ),
            await db.QuerySingleAsync<string>(
                "SELECT coalesce(revoked_at::text, 'active') FROM casebox.api_tokens WHERE id = @Id",
                new { Id = a.TokenId }
            ),
            await db.QuerySingleAsync<string>(
                "SELECT status || attempts FROM casebox.jobs WHERE id = @Id",
                new { Id = a.JobId }
            ),
            await db.QuerySingleAsync<string>(
                "SELECT role FROM casebox.accounts WHERE id = @Id",
                new { Id = a.AccountId }
            ),
            await db.QuerySingleAsync<string>(
                "SELECT count(*)::text FROM deedbox.events WHERE tenant_id = @Org AND stream_id = @Stream",
                new { Org = StackFixture.OrgA, Stream = $"workspace:{a.Workspace}" }
            ),
            await db.QuerySingleAsync<string>(
                "SELECT count(*)::text FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id",
                new { Org = StackFixture.OrgA, Id = a.SessionId }
            ),
        };
        return string.Join('|', parts);
    }
}
