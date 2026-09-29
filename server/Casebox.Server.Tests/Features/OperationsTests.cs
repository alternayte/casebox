using Casebox.Server.Features.Demo;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Steering;
using Casebox.Server.Tests.Infrastructure;
using Deedbox;
using Microsoft.Extensions.DependencyInjection;

namespace Casebox.Server.Tests.Features;

// Step 13 (docs/specs/operations.md): metrics on the management port only.
public sealed class OperationsTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task Metrics_answer_on_the_management_port_only()
    {
        using var management = stack.ServerA.Management();
        var deadline = DateTime.UtcNow.AddSeconds(30);
        string metrics;
        // The first scrape can come before the exporter has collected once.
        do
        {
            metrics = await management.GetStringAsync("/metrics", Ct);
            if (metrics.Contains("casebox_workers_seen", StringComparison.Ordinal))
                break;
            await Task.Delay(500, Ct);
        } while (DateTime.UtcNow < deadline);
        Assert.Contains("casebox_workers_seen", metrics);
        Assert.Contains("deedbox_", metrics);
        Assert.DoesNotContain("person:", metrics);

        var outside = await stack.ServerA.Anonymous().GetAsync("/metrics", Ct);
        Assert.DoesNotContain("casebox_workers_seen", await outside.Content.ReadAsStringAsync(Ct));
    }
}

// `casebox up --demo`: the synthetic team fills the report, the cases, an evaluation and a
// proposal through the real streams, and loads only once.
public sealed class DemoTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task The_demo_team_fills_every_page_once()
    {
        var org = $"demo-{Guid.NewGuid():N}"[..20];
        using var scope = stack.ServerA.Services.CreateScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = org;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        await store.Execute<Organisation>(
            Organisation.StreamId,
            o => OrgDecider.Create(o, "Demo"),
            Ct
        );
        var seed = scope.ServiceProvider.GetRequiredService<DemoSeed>();

        Assert.True(await seed.SeedAsync(Ct));
        Assert.False(await seed.SeedAsync(Ct));

        var report = await scope
            .ServiceProvider.GetRequiredService<SteeringReports>()
            .BuildAsync(
                new ReportQuery(
                    DateTimeOffset.UtcNow.AddDays(-90),
                    DateTimeOffset.UtcNow.AddDays(1),
                    null,
                    null,
                    null,
                    null,
                    null,
                    null,
                    null
                ),
                Ct
            );
        Assert.True(report.Themes.Count >= 4, $"themes: {report.Themes.Count}");
        Assert.All(report.Themes, t => Assert.True(t.People >= 3));
        Assert.NotNull(report.Headline.CorrectionFreeRate);
        Assert.Contains(report.Themes, t => t.Patterns is { Count: > 0 });

        await using var db = new Npgsql.NpgsqlConnection(stack.ConnectionString);
        var proposals = (
            await Dapper.SqlMapper.QueryAsync<(string Kind, string Status)>(
                db,
                "SELECT kind, status FROM casebox.proposals WHERE org_id = @Org ORDER BY kind",
                new { Org = org }
            )
        ).ToList();
        // One of each kind a person meets: applied privately, and two waiting for a decision.
        Assert.Equal(
            [("code_note", "open"), ("harness_edit", "applied"), ("skill", "open")],
            proposals
        );
        Assert.Equal(
            1,
            await Dapper.SqlMapper.ExecuteScalarAsync<int>(
                db,
                "SELECT count(*) FROM casebox.patterns WHERE org_id = @Org AND advisory_note IS NOT NULL",
                new { Org = org }
            )
        );
    }
}
