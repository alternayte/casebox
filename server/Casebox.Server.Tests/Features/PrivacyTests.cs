using System.Net.Http.Json;
using System.Text;
using System.Text.RegularExpressions;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Tokens;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Deedbox;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

public sealed class KRuleTests
{
    private static (string, Person) Row(string key, string token, bool mapped = true) =>
        (key, new Person(token, mapped));

    [Fact]
    public void A_group_needs_k_distinct_mapped_people()
    {
        var groups = KRule.Apply(
            [
                Row("tests", "p1"),
                Row("tests", "p1"),
                Row("tests", "p2"),
                Row("tests", "p3"),
                Row("docs", "p1"),
                Row("docs", "p2"),
            ],
            3
        );
        var shown = Assert.Single(groups);
        Assert.Equal(new KGroup<string>("tests", 3, 4), shown);
    }

    [Fact]
    public void Unmapped_tokens_never_count_toward_k()
    {
        // One person split into an unmapped email token and a mapped token looks like two people.
        var groups = KRule.Apply(
            [Row("tests", "p1"), Row("tests", "p2"), Row("tests", "email-token", mapped: false)],
            3
        );
        Assert.Empty(groups);
    }

    [Fact]
    public void K_never_drops_below_two() =>
        Assert.Throws<ArgumentOutOfRangeException>(() => KRule.Apply([Row("tests", "p1")], 1));
}

public sealed partial class PrivacyTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [GeneratedRegex("person:[a-z2-7]{26}")]
    private static partial Regex Token();

    // The solo trial: the only person sees their own data without k. A second person in the data,
    // or a second account, ends it for good, even after the second person's data is gone.
    [Theory]
    [InlineData("person")]
    [InlineData("account")]
    public async Task Solo_shows_the_only_person_their_own_data_until_a_second_person_or_account_arrives(
        string second
    )
    {
        var org = $"org-solo-{Guid.NewGuid():N}"[..20];
        await using var scope = stack.ServerA.Services.CreateAsyncScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = org;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        var solo = scope.ServiceProvider.GetRequiredService<Solo>();
        await store.Execute<Organisation>(
            Organisation.StreamId,
            state => OrgDecider.Create(state, "Solo"),
            Ct
        );
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var now = DateTimeOffset.UtcNow;
        async Task SessionAsync(string id, string person, string period) =>
            await db.ExecuteAsync(
                """
                INSERT INTO casebox.sessions (org_id, id, agent, person, source, started_at, created_at, updated_at, period)
                VALUES (@Org, @Id, 'cursor-cli', @Person, 'import', @Now, @Now, @Now, @Period)
                """,
                new
                {
                    Org = org,
                    Id = id,
                    Person = person,
                    Now = now,
                    Period = period,
                }
            );
        async Task AccountAsync(string id) =>
            await db.ExecuteAsync(
                """
                INSERT INTO casebox.accounts (id, org_id, issuer, subject, display_name, role, created_at, last_login_at)
                VALUES (@Id, @Org, 'local', @Id, 'someone', 'owner', @Now, @Now)
                """,
                new
                {
                    Id = $"{org}-{id}",
                    Org = org,
                    Now = now,
                }
            );

        await AccountAsync("me");
        await SessionAsync("s1", "person:me-q3", "2026-Q3");
        // A new period gives the same person a new token; that is still one person.
        await SessionAsync("s2", "person:me-q4", "2026-Q4");
        var view = await solo.ViewAsync(Ct);
        Assert.True(view.Solo);
        var mine = new[] { new Person("person:me-q3", Mapped: false, "2026-Q3") };
        Assert.True(KRule.Meets(mine, view));
        Assert.Equal(1, KRule.People(mine, view));

        if (second == "person")
            await SessionAsync("s3", "person:other-q4", "2026-Q4");
        else
            await AccountAsync("teammate");
        view = await solo.ViewAsync(Ct);
        Assert.False(view.Solo);
        Assert.False(KRule.Meets(mine, view));

        await db.ExecuteAsync(
            "DELETE FROM casebox.sessions WHERE org_id = @Org AND id = 's3'; DELETE FROM casebox.accounts WHERE org_id = @Org AND id <> @Me",
            new { Org = org, Me = $"{org}-me" }
        );
        Assert.False((await solo.ViewAsync(Ct)).Solo);
        Assert.Equal(
            1,
            await db.ExecuteScalarAsync<int>(
                "SELECT count(*)::int FROM deedbox.events WHERE tenant_id = @Org AND event_type = 'org.solo_ended'",
                new { Org = org }
            )
        );
    }

    [Fact]
    public async Task The_roster_gives_one_person_one_mapped_token_and_leaves_strangers_unmapped()
    {
        await SetPromptModeAsync();
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var now = DateTimeOffset.UtcNow;
        var byEmail = await IngestAsync(ingest, "⟦cbx:email:Ada.Roster@example.com⟧", now);
        var byLogin = await IngestAsync(ingest, "⟦cbx:github:ada-roster⟧", now);
        var stranger = await IngestAsync(
            ingest,
            $"⟦cbx:email:stranger-{Guid.NewGuid():N}@example.com⟧",
            now
        );

        var sessions = await SessionsAsync(byEmail, byLogin, stranger);
        Assert.Equal(sessions[byEmail].Person, sessions[byLogin].Person);
        Assert.True(sessions[byEmail].Mapped);
        Assert.False(sessions[stranger].Mapped);
    }

    [Fact]
    public async Task Erasing_an_identity_deletes_its_sessions_in_every_period_and_audits_without_the_identity()
    {
        await SetPromptModeAsync();
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var eve = $"eve-{Guid.NewGuid():N}@example.com";
        var now = DateTimeOffset.UtcNow;
        var thisQuarter = await IngestAsync(ingest, $"⟦cbx:email:{eve}⟧", now);
        var lastQuarter = await IngestAsync(ingest, $"⟦cbx:email:{eve}⟧", now.AddMonths(-3));
        var bob = await IngestAsync(ingest, $"⟦cbx:email:bob-{Guid.NewGuid():N}@example.com⟧", now);

        var admin = await stack.ServerA.AdminAsync();
        var result = await (
            await admin.PostAsJsonAsync(
                "/api/v1/privacy/erasures",
                new { identity = $"email:{eve}" },
                Ct
            )
        ).Content.ReadFromJsonAsync<ErasureResult>(Ct);
        Assert.True(result!.Subjects >= 2);
        Assert.Equal(2, result.Sessions);

        var left = await SessionsAsync(thisQuarter, lastQuarter, bob);
        Assert.Equal([bob], left.Keys);
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        Assert.Equal(
            0,
            await db.QuerySingleAsync<int>(
                "SELECT count(*) FROM casebox.session_events WHERE session_id = ANY(@Ids)",
                new { Ids = new[] { thisQuarter, lastQuarter } }
            )
        );
        var audits = await db.QueryAsync<string>(
            "SELECT payload::text FROM deedbox.events WHERE tenant_id = @Org AND event_type = 'org.erasure_performed'",
            new { Org = StackFixture.OrgA }
        );
        Assert.All(audits, a => Assert.DoesNotContain("eve-", a, StringComparison.Ordinal));

        var again = await (
            await admin.PostAsJsonAsync(
                "/api/v1/privacy/erasures",
                new { identity = $"email:{eve}" },
                Ct
            )
        ).Content.ReadFromJsonAsync<ErasureResult>(Ct);
        Assert.Equal(0, again!.Sessions);
    }

    [Fact]
    public async Task A_period_past_the_retention_window_is_retired_and_old_trace_events_are_deleted()
    {
        await SetPromptModeAsync();
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var identity = $"email:old-{Guid.NewGuid():N}@example.com";
        var old = new DateTimeOffset(2024, 2, 10, 12, 0, 0, TimeSpan.Zero);
        var session = await IngestAsync(ingest, $"⟦cbx:{identity}⟧", old);

        await stack
            .ServerA.Services.GetRequiredService<Retention>()
            .RunForAsync(StackFixture.OrgA, Ct);

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        Assert.True(
            await db.QuerySingleAsync<bool>(
                "SELECT period_retired_at IS NOT NULL FROM casebox.sessions WHERE id = @Id",
                new { Id = session }
            )
        );
        Assert.Equal(
            0,
            await db.QuerySingleAsync<int>(
                "SELECT count(*) FROM casebox.session_events WHERE session_id = @Id",
                new { Id = session }
            )
        );
        await using var scope = stack.ServerA.Services.CreateAsyncScope();
        scope.ServiceProvider.GetRequiredService<DeedboxContext>().TenantId = StackFixture.OrgA;
        var pseudonyms = scope.ServiceProvider.GetRequiredService<IPseudonyms>();
        var destroyed = await Assert.ThrowsAsync<DeedboxException>(() =>
            pseudonyms.SubjectForAsync(identity, "2024-Q1", Ct)
        );
        Assert.Equal("DBX036", destroyed.Code);
    }

    // SDD section 14: ingest fixtures with known identities, then a scan of every table and blob.
    [Fact]
    public async Task No_identity_reaches_any_table_or_blob()
    {
        await SetPromptModeAsync();
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        var tag = Guid.NewGuid().ToString("N")[..10];
        var email = $"scan.{tag}@example.com";
        var login = $"scan-login-{tag}";
        var name = $"Scanname {tag}";
        var account = $"acct-{tag}";
        var other = $"other.{tag}@example.com";
        var now = DateTimeOffset.UtcNow;

        var batch = new CaptureBatch(
            new CapturedSession(
                $"claude-code:{Guid.NewGuid()}",
                "claude-code",
                "2.1.284",
                null,
                "github.com/acme/app",
                "main",
                null,
                null,
                now,
                null,
                "import",
                null,
                $"⟦cbx:email:{email}⟧"
            ),
            [
                new CapturedEvent(
                    0,
                    now,
                    "prompt",
                    $"ask ⟦cbx:github:{login}⟧ and ⟦cbx:name:{name}⟧, cc {other}",
                    null,
                    null,
                    new Dictionary<string, string> { ["note"] = $"from {other}" }
                ),
                new CapturedEvent(
                    1,
                    now,
                    "tool_result",
                    $"git log says {name} <{email}>",
                    new CapturedTool("Bash", "ok", null),
                    null,
                    null
                ),
            ]
        );
        (
            await ingest.PostAsJsonAsync("/ingest/v1/sessions", batch, Json.Options, Ct)
        ).EnsureSuccessStatusCode();

        object Attr(string k, string v) => new { key = k, value = new { stringValue = v } };
        var nanos = (now.ToUnixTimeMilliseconds() * 1_000_000).ToString(
            System.Globalization.CultureInfo.InvariantCulture
        );
        var resource = new
        {
            attributes = new[]
            {
                Attr("user.email", email),
                Attr("user.account_uuid", account),
                Attr("session.id", tag),
            },
        };
        (
            await ingest.PostAsJsonAsync(
                "/v1/logs",
                new
                {
                    resourceLogs = new[]
                    {
                        new
                        {
                            resource,
                            scopeLogs = new[]
                            {
                                new
                                {
                                    logRecords = new[]
                                    {
                                        new
                                        {
                                            timeUnixNano = nanos,
                                            attributes = new[]
                                            {
                                                Attr("event.name", "claude_code.user_prompt"),
                                                Attr("event.sequence", "1"),
                                                Attr("prompt", $"tell {other}"),
                                            },
                                        },
                                    },
                                },
                            },
                        },
                    },
                },
                Ct
            )
        ).EnsureSuccessStatusCode();
        (
            await ingest.PostAsJsonAsync(
                "/v1/metrics",
                new
                {
                    resourceMetrics = new[]
                    {
                        new
                        {
                            resource,
                            scopeMetrics = new[]
                            {
                                new
                                {
                                    metrics = new[]
                                    {
                                        new
                                        {
                                            name = "claude_code.token.usage",
                                            sum = new
                                            {
                                                dataPoints = new[]
                                                {
                                                    new
                                                    {
                                                        timeUnixNano = nanos,
                                                        asDouble = 12.0,
                                                        attributes = new[]
                                                        {
                                                            Attr("user.email", email),
                                                        },
                                                    },
                                                },
                                            },
                                        },
                                    },
                                },
                            },
                        },
                    },
                },
                Ct
            )
        ).EnsureSuccessStatusCode();

        var needles = new[] { email, login, name, account, other }
            .Select(n => n.ToLowerInvariant())
            .ToArray();
        var found = await PrivacyScan.FindAsync(stack, needles);
        Assert.True(found.Count == 0, "Identities found in: " + string.Join(", ", found));
    }

    [Fact]
    public async Task No_route_returns_a_person_token()
    {
        await SetPromptModeAsync();
        var ingest = await stack.ServerA.TokenClientAsync(TokenKind.Ingest);
        await IngestAsync(
            ingest,
            $"⟦cbx:email:route-{Guid.NewGuid():N}@example.com⟧",
            DateTimeOffset.UtcNow
        );

        var admin = await stack.ServerA.AdminAsync();
        var worker = await stack.ServerA.TokenClientAsync(TokenKind.Worker);
        var routes = stack
            .ServerA.Services.GetRequiredService<Microsoft.AspNetCore.Routing.EndpointDataSource>()
            .Endpoints.OfType<Microsoft.AspNetCore.Routing.RouteEndpoint>()
            .Where(e =>
                e.Metadata.GetMetadata<Microsoft.AspNetCore.Routing.HttpMethodMetadata>()
                    ?.HttpMethods.Contains("GET") == true
            )
            .Select(e => e.RoutePattern.RawText!)
            .Where(p =>
                !p.Contains('{', StringComparison.Ordinal)
                && (
                    p.StartsWith("/api/v1", StringComparison.Ordinal)
                    || p.StartsWith("/worker/v1", StringComparison.Ordinal)
                    || p.StartsWith("/ingest/v1", StringComparison.Ordinal)
                )
            )
            .ToList();
        Assert.NotEmpty(routes);
        foreach (var route in routes)
        {
            var client =
                route.StartsWith("/worker", StringComparison.Ordinal) ? worker
                : route.StartsWith("/ingest", StringComparison.Ordinal) ? ingest
                : admin;
            var body = await (await client.GetAsync(route, Ct)).Content.ReadAsStringAsync(Ct);
            Assert.False(Token().IsMatch(body), $"GET {route} returned a person token.");
        }
    }

    private static async Task<string> IngestAsync(
        HttpClient ingest,
        string person,
        DateTimeOffset at
    )
    {
        var id = $"codex:{Guid.NewGuid()}";
        var batch = new CaptureBatch(
            new CapturedSession(
                id,
                "codex",
                "0.153.4",
                null,
                "github.com/acme/app",
                "main",
                null,
                null,
                at,
                null,
                "import",
                null,
                person
            ),
            [new CapturedEvent(0, at, "prompt", "fix it", null, null, null)]
        );
        (
            await ingest.PostAsJsonAsync("/ingest/v1/sessions", batch, Json.Options, Ct)
        ).EnsureSuccessStatusCode();
        return id;
    }

    private async Task<Dictionary<string, (string Person, bool Mapped)>> SessionsAsync(
        params string[] ids
    )
    {
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var rows = await db.QueryAsync<(string Id, string Person, bool Mapped)>(
            "SELECT id, person, person_mapped FROM casebox.sessions WHERE org_id = @Org AND id = ANY(@Ids)",
            new { Org = StackFixture.OrgA, Ids = ids }
        );
        return rows.ToDictionary(r => r.Id, r => (r.Person, r.Mapped));
    }

    private async Task SetPromptModeAsync()
    {
        var admin = await stack.ServerA.AdminAsync();
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>(
            "/api/v1/org",
            Json.Options,
            Ct
        );
        if (org!.Settings.PromptMode is null)
            (
                await admin.PutAsJsonAsync(
                    "/api/v1/org/settings",
                    org.Settings with
                    {
                        PromptMode = PromptMode.Redacted,
                    },
                    Json.Options,
                    Ct
                )
            ).EnsureSuccessStatusCode();
    }
}
