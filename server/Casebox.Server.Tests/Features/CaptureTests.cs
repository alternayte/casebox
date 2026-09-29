using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

public sealed class CaptureTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task A_device_login_gives_the_CLI_a_token_that_acts_as_the_approving_account_once()
    {
        var cli = stack.ServerA.Anonymous();
        var code = await (await cli.PostAsJsonAsync("/api/v1/auth/device/code", new { }, Ct)).Content.ReadFromJsonAsync<DeviceLogin.CodeResponse>(Ct);
        Assert.Equal(HttpStatusCode.PreconditionRequired, (await cli.PostAsJsonAsync("/api/v1/auth/device/token", new { code!.DeviceCode }, Ct)).StatusCode);

        var admin = await stack.ServerA.AdminAsync();
        Assert.Equal(HttpStatusCode.NoContent, (await admin.PostAsJsonAsync("/api/v1/auth/device/approve", new { userCode = code.UserCode.ToLowerInvariant().Replace("-", "") }, Ct)).StatusCode);

        var token = await (await cli.PostAsJsonAsync("/api/v1/auth/device/token", new { code.DeviceCode }, Ct)).Content.ReadFromJsonAsync<DeviceLogin.TokenResponse>(Ct);
        Assert.StartsWith("cbx_cli_", token!.AccessToken, StringComparison.Ordinal);
        Assert.Equal(HttpStatusCode.BadRequest, (await cli.PostAsJsonAsync("/api/v1/auth/device/token", new { code.DeviceCode }, Ct)).StatusCode);

        var asCli = stack.ServerA.Anonymous();
        asCli.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", token.AccessToken);
        var me = await asCli.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);
        Assert.Equal(Role.Owner, me!.Role);
        var device = await asCli.PostAsync("/api/v1/devices", null, Ct);
        Assert.Equal(HttpStatusCode.Created, device.StatusCode);
    }

    [Fact]
    public async Task Capture_is_off_until_an_admin_chooses_a_prompt_mode()
    {
        await using var scope = stack.ServerA.Services.CreateAsyncScope();
        var capture = scope.ServiceProvider.GetRequiredService<CaptureStore>();
        await Assert.ThrowsAsync<ConflictException>(() => capture.StoreAsync(StackFixture.OrgA, OrgSettings.Defaults, Batch("off-probe", []), Ct));
    }

    [Fact]
    public async Task Ingest_redacts_secrets_tokenizes_identities_keeps_only_what_the_mode_allows_and_stores_each_event_once()
    {
        await SetPromptModeAsync("redacted");
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var id = $"claude-code:{Guid.NewGuid()}";
        var at = DateTimeOffset.UtcNow;
        var batch = Batch(id,
        [
            new CapturedEvent(0, at, "prompt", "Ask ⟦cbx:name:Ada Lovelace⟧ or mail grace@example.com; key AKIAIOSFODNN7EXAMPLE", null, null, null),
            new CapturedEvent(1, at, "tool_result", "file contents with a secret", new CapturedTool("Read", "ok", ["/Users/ada/src/app.go"]), null, null),
        ]);

        var first = await ingest.PostAsJsonAsync("/ingest/v1/sessions", batch, Json.Options, Ct);
        Assert.Equal(HttpStatusCode.OK, first.StatusCode);
        var again = await (await ingest.PostAsJsonAsync("/ingest/v1/sessions", batch, Json.Options, Ct)).Content.ReadFromJsonAsync<IngestResult>(Ct);
        Assert.Equal(0, again!.Stored);

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var person = await db.QuerySingleAsync<string>("SELECT person FROM casebox.sessions WHERE org_id = @Org AND id = @Id", new { Org = StackFixture.OrgA, Id = id });
        Assert.StartsWith("person:", person, StringComparison.Ordinal);
        var rows = (await db.QueryAsync<(long Seq, string? Text, string? Tool)>(
            "SELECT seq, text, tool::text FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id ORDER BY seq", new { Org = StackFixture.OrgA, Id = id })).ToList();
        Assert.Equal(2, rows.Count);
        var prompt = rows[0].Text!;
        foreach (var raw in new[] { "Ada Lovelace", "grace@example.com", "AKIAIOSFODNN7EXAMPLE", "dev@example.com" })
            Assert.DoesNotContain(raw, prompt, StringComparison.Ordinal);
        Assert.Contains("[redacted:aws_key]", prompt, StringComparison.Ordinal);
        Assert.Equal(2, System.Text.RegularExpressions.Regex.Matches(prompt, "person:[a-z2-7]{26}").Count);
        Assert.Null(rows[1].Text);
        Assert.DoesNotContain("/Users/ada", rows[1].Tool!, StringComparison.Ordinal);
    }

    [Fact]
    public async Task Native_telemetry_becomes_session_events_and_metrics_without_storing_the_email()
    {
        await SetPromptModeAsync("redacted");
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var session = Guid.NewGuid().ToString();
        var email = $"otel-{Guid.NewGuid():N}@example.com";
        var nanos = (DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() * 1_000_000).ToString(System.Globalization.CultureInfo.InvariantCulture);
        object Attr(string k, string v) => new { key = k, value = new { stringValue = v } };
        var logs = new
        {
            resourceLogs = new[]
            {
                new
                {
                    resource = new { attributes = new[] { Attr("service.name", "claude-code"), Attr("user.email", email), Attr("session.id", session) } },
                    scopeLogs = new[]
                    {
                        new
                        {
                            logRecords = new object[]
                            {
                                new { timeUnixNano = nanos, body = new { stringValue = "claude_code.user_prompt" }, attributes = new[] { Attr("event.name", "user_prompt"), Attr("event.sequence", "3"), Attr("prompt", "please fix the tests") } },
                                new { timeUnixNano = nanos, body = new { stringValue = "claude_code.api_request" }, attributes = new[] { Attr("event.name", "api_request"), Attr("event.sequence", "4"), Attr("model", "claude-sonnet-5"), Attr("input_tokens", "1200"), Attr("output_tokens", "300"), Attr("cost_usd", "0.012") } },
                                new { timeUnixNano = nanos, body = new { stringValue = "claude_code.tool_decision" }, attributes = new[] { Attr("event.name", "tool_decision"), Attr("event.sequence", "5"), Attr("tool_name", "Bash"), Attr("decision", "reject"), Attr("source", "user_reject") } },
                            },
                        },
                    },
                },
            },
        };
        for (var i = 0; i < 2; i++)
            (await ingest.PostAsJsonAsync("/v1/logs", logs, Ct)).EnsureSuccessStatusCode();

        var metrics = new
        {
            resourceMetrics = new[]
            {
                new
                {
                    resource = new { attributes = new[] { Attr("service.name", "claude-code"), Attr("user.email", email), Attr("session.id", session) } },
                    scopeMetrics = new[] { new { metrics = new[] { new { name = "claude_code.cost.usage", sum = new { dataPoints = new[] { new { timeUnixNano = nanos, asDouble = 0.012, attributes = new[] { Attr("model", "claude-sonnet-5") } } } } } } } },
                },
            },
        };
        (await ingest.PostAsJsonAsync("/v1/metrics", metrics, Ct)).EnsureSuccessStatusCode();

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var kinds = (await db.QueryAsync<(long Seq, string Kind)>(
            "SELECT seq, kind FROM casebox.session_events WHERE org_id = @Org AND session_id = @Id ORDER BY seq", new { Org = StackFixture.OrgA, Id = $"claude-code:{session}" })).ToList();
        Assert.Equal([(Otlp.SeqBase + 3, "prompt"), (Otlp.SeqBase + 4, "api_request"), (Otlp.SeqBase + 5, "denial")], kinds);

        var leaks = await db.QuerySingleAsync<int>(
            """
            SELECT (SELECT count(*) FROM casebox.sessions WHERE row_to_json(sessions)::text LIKE @Like)
                 + (SELECT count(*) FROM casebox.session_events WHERE row_to_json(session_events)::text LIKE @Like)
                 + (SELECT count(*) FROM casebox.session_metrics WHERE row_to_json(session_metrics)::text LIKE @Like)
            """,
            new { Like = $"%{email}%" });
        Assert.Equal(0, leaks);
        Assert.Equal(1, await db.QuerySingleAsync<int>("SELECT count(*) FROM casebox.session_metrics WHERE session_id = @Id", new { Id = session }));
    }

    private sealed record IngestResult(int Accepted, int Stored);

    private static CaptureBatch Batch(string id, IReadOnlyList<CapturedEvent> events) =>
        new(new CapturedSession(id, "claude-code", "2.1.284", "claude-sonnet-5", "github.com/acme/app", "main", null, null,
            DateTimeOffset.UtcNow, null, "import", null, "⟦cbx:email:dev@example.com⟧"), events);

    private async Task SetPromptModeAsync(string mode)
    {
        var admin = await stack.ServerA.AdminAsync();
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>("/api/v1/org", Json.Options, Ct);
        if (org!.Settings.PromptMode is null)
            (await admin.PutAsJsonAsync("/api/v1/org/settings", org.Settings with { PromptMode = Enum.Parse<PromptMode>(mode, true) }, Json.Options, Ct)).EnsureSuccessStatusCode();
    }
}
