using System.Net;
using System.Net.Http.Json;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Workspaces;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// The wiring of SDD section 10, end to end through the real QueueBox and deploy/queuebox.yml.
public sealed class QueueBoxTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task QueueBox_delivers_an_outbox_row_to_the_effects_endpoint_of_its_kind()
    {
        var marker = Guid.NewGuid().ToString();
        await using (var db = new NpgsqlConnection(stack.ConnectionString))
        {
            await db.ExecuteAsync(
                "INSERT INTO outbox (topic, key, payload, headers) VALUES ('effect.notify', @Key, @Payload::jsonb, '{}')",
                new { Key = marker, Payload = $$"""{"org":"{{StackFixture.OrgA}}","marker":"{{marker}}"}""" });
        }

        var delivered = await Eventually(() => stack.ServerA.Effects.Received.FirstOrDefault(m => m.Payload.GetProperty("marker").GetString() == marker));
        Assert.NotNull(delivered);
        Assert.Equal("notify", delivered.Kind);
        Assert.False(string.IsNullOrEmpty(delivered.MessageId));
    }

    [Fact]
    public async Task The_effects_endpoint_answers_only_QueueBox_and_only_on_the_management_port()
    {
        using var management = stack.ServerA.Management();
        Assert.Equal(HttpStatusCode.Unauthorized, (await management.PostAsJsonAsync("/internal/effects/notify", new { org = "x" }, Ct)).StatusCode);

        var wrongPort = stack.ServerA.Anonymous();
        wrongPort.DefaultRequestHeaders.Add(EffectEndpoints.TokenHeader, StackFixture.EffectsToken);
        var response = await wrongPort.PostAsJsonAsync("/internal/effects/notify", new { org = "x" }, Ct);
        Assert.NotEqual(HttpStatusCode.NoContent, response.StatusCode);
    }

    [Fact]
    public async Task A_repeated_poll_of_the_same_state_is_handled_once()
    {
        var name = $"poll-{Guid.NewGuid():N}"[..20];
        var publisher = stack.ServerA.Services.GetRequiredService<PollPublisher>();
        var message = new PollMessage($"test:{name}@2026-09-29T10:00:00Z", "test.create_workspace", StackFixture.OrgA, new { workspace = name });

        await publisher.PublishAsync(message, Ct);
        await publisher.PublishAsync(message, Ct);

        Assert.True(await Eventually(async () => await WorkspaceExistsAsync(name)));
        await Task.Delay(2000, Ct);
        Assert.Equal(1, TestInboxHandler.Attempts[name]);
        Assert.Equal(1, await WorkerTests.EventCountAsync(stack.ConnectionString, StackFixture.OrgA, Workspace.StreamIdFor(name), "workspace.created"));
    }

    [Fact]
    public async Task A_failing_inbox_handler_leaves_no_events_and_the_row_is_retried()
    {
        var name = $"fail-{Guid.NewGuid():N}"[..20];
        var publisher = stack.ServerA.Services.GetRequiredService<PollPublisher>();
        await publisher.PublishAsync(new PollMessage($"test:{name}@1", "test.create_workspace", StackFixture.OrgA, new { workspace = name, fail = true }), Ct);

        Assert.True(await Eventually(() => Task.FromResult(TestInboxHandler.Attempts.GetValueOrDefault(name) >= 2)));
        Assert.False(await WorkspaceExistsAsync(name));
    }

    [Fact]
    public async Task The_poll_source_refuses_a_request_without_its_token()
    {
        using var http = new HttpClient();
        var response = await http.PostAsJsonAsync(new Uri(stack.QueueBoxUrl, "/inbox/poll"), new { key = "k", type = "t", org = "o", payload = new { } }, Ct);
        Assert.Equal(HttpStatusCode.Unauthorized, response.StatusCode);
    }

    [Fact]
    public async Task Readiness_covers_the_database_Deedbox_and_QueueBox()
    {
        using var management = stack.ServerA.Management();
        var ready = await management.GetAsync("/healthz/ready", Ct);
        Assert.Equal(HttpStatusCode.OK, ready.StatusCode);
        Assert.Equal(HttpStatusCode.OK, (await management.GetAsync("/healthz/live", Ct)).StatusCode);
    }

    [Fact]
    public async Task The_OpenAPI_document_describes_the_API()
    {
        var document = await stack.ServerA.Anonymous().GetStringAsync("/openapi/v1.json", Ct);
        Assert.Contains("/api/v1/workspaces", document, StringComparison.Ordinal);
        Assert.Contains("/worker/v1/jobs/lease", document, StringComparison.Ordinal);
        Assert.DoesNotContain("/internal/effects", document, StringComparison.Ordinal);
    }

    private async Task<bool> WorkspaceExistsAsync(string name)
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        return await db.QuerySingleAsync<bool>(
            "SELECT EXISTS (SELECT 1 FROM casebox.workspaces WHERE org_id = @Org AND name = @Name)", new { Org = StackFixture.OrgA, Name = name });
    }

    private static async Task<T?> Eventually<T>(Func<T?> probe) => await Eventually(() => Task.FromResult(probe()));

    private static async Task<T?> Eventually<T>(Func<Task<T?>> probe)
    {
        var deadline = DateTime.UtcNow.AddSeconds(30);
        while (true)
        {
            var value = await probe();
            if (value is not null && !Equals(value, default(T)) || DateTime.UtcNow > deadline) return value;
            await Task.Delay(200, Ct);
        }
    }
}
