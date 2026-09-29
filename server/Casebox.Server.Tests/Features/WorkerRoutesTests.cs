using System.Net.Http.Json;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Npgsql;

namespace Casebox.Server.Tests.Features;

public sealed class RosterMergeTests
{
    // A GitHub login and a Jira account that share an email are one person, named by the login.
    [Fact]
    public void People_who_share_an_identity_across_sources_are_one_person()
    {
        var map = Roster.Merge([
            new RosterPerson("github:alice", ["email:alice@example.com", "name:Alice Walker"]),
            new RosterPerson("jira:awalker", ["email:Alice@Example.com"]),
            new RosterPerson("jira:bob", ["email:bob@example.com"]),
        ]);
        Assert.Equal("github:alice", map["jira:awalker"]);
        Assert.Equal("github:alice", map["email:alice@example.com"]);
        Assert.Equal("github:alice", map["name:Alice Walker"]);
        Assert.Equal("jira:bob", map["email:bob@example.com"]);
    }
}

public sealed class WorkerRoutesTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task A_git_ai_attribution_marks_its_pull_request_as_an_agent_pull_request()
    {
        var sha = FakePull.Sha();
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.ExecuteAsync(
            """
            INSERT INTO casebox.pull_requests (org_id, repo, number, state, head_ref, base_ref, author, updated_at, snapshot)
            VALUES (@Org, 'github.com/acme/attr', 5, 'open', 'feature', 'main', 'person:x', now(), @Snapshot::jsonb)
            """,
            new { Org = StackFixture.OrgA, Snapshot = $$"""{"commits":[{"sha":"{{sha}}"}]}""" });

        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        (await worker.PostAsJsonAsync("/worker/v1/attributions", new
        {
            repo = "github.com/acme/attr",
            // Real notes can hold thousands of ranges for one file; they must fit.
            commits = new[] { new { sha, files = new[] {
                new { path = "src/a.go", agent = "claude", model = "claude-sonnet-5", ranges = string.Join(',', Enumerable.Range(0, 1500).Select(i => $"{i * 3 + 1}")) },
            } } },
        }, Ct)).EnsureSuccessStatusCode();

        Assert.True(await db.QuerySingleAsync<bool>("SELECT is_agent FROM casebox.pull_requests WHERE org_id = @Org AND repo = 'github.com/acme/attr' AND number = 5", new { Org = StackFixture.OrgA }));
        Assert.Equal(1, await db.QuerySingleAsync<int>("SELECT count(*) FROM casebox.commit_attributions WHERE org_id = @Org AND sha = @Sha", new { Org = StackFixture.OrgA, Sha = sha }));
    }

    [Fact]
    public async Task An_Entire_session_is_skipped_when_local_capture_already_has_it()
    {
        var admin = await stack.ServerA.AdminAsync();
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>("/api/v1/org", Json.Options, Ct);
        if (org!.Settings.PromptMode is null)
            (await admin.PutAsJsonAsync("/api/v1/org/settings", org.Settings with { PromptMode = PromptMode.Redacted }, Json.Options, Ct)).EnsureSuccessStatusCode();

        var id = $"claude-code:{Guid.NewGuid()}";
        var at = DateTimeOffset.UtcNow;
        CaptureBatch Batch(string source, long seq) => new(
            new CapturedSession(id, "claude-code", null, null, "github.com/acme/app", "main", null, null, at, null, source, null, "⟦cbx:email:dev@example.com⟧"),
            [new CapturedEvent(seq, at, "prompt", "fix it", null, null, null)]);

        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        var first = await (await worker.PostAsJsonAsync("/worker/v1/sessions", Batch("entire", 3_000_000_000), Json.Options, Ct)).Content.ReadFromJsonAsync<SessionResult>(Ct);
        Assert.False(first!.Skipped);

        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        (await ingest.PostAsJsonAsync("/ingest/v1/sessions", Batch("import", 0), Json.Options, Ct)).EnsureSuccessStatusCode();
        var again = await (await worker.PostAsJsonAsync("/worker/v1/sessions", Batch("entire", 3_000_000_001), Json.Options, Ct)).Content.ReadFromJsonAsync<SessionResult>(Ct);
        Assert.True(again!.Skipped);
    }

    private sealed record SessionResult(int Stored, bool Skipped);
}
