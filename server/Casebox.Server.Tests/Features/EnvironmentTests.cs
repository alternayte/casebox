using System.Net.Http.Json;
using System.Text.Json;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// docs/specs/sandboxes.md: a confirmed recipe gets one env.build job per workspace repository,
// carrying the recipe as proposed.
public sealed class EnvironmentTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task A_confirmed_recipe_is_prepared_on_the_workers_once_per_repository()
    {
        var admin = await stack.ServerA.AdminAsync();
        var name = $"env-{Guid.NewGuid():N}"[..20];
        (await admin.PostAsJsonAsync("/api/v1/workspaces", new { name }, Ct)).EnsureSuccessStatusCode();
        foreach (var repo in new[] { "github.com/acme/env-api", "github.com/acme/env-worker" })
            (await admin.PostAsJsonAsync($"/api/v1/workspaces/{name}/repos", new { repo }, Ct)).EnsureSuccessStatusCode();

        var recipe = JsonDocument.Parse("""{"image":"golang:1.26","install":["go mod download"],"lockfiles":["go.mod","go.sum"],"test":[{"command":"go test ./..."}],"services":{},"links":[]}""").RootElement;
        var proposed = await (await admin.PutAsJsonAsync($"/api/v1/workspaces/{name}/recipe", new { recipe }, Ct)).Content.ReadFromJsonAsync<JsonElement>(Ct);
        var hash = proposed.GetProperty("recipeHash").GetString()!;
        (await admin.PostAsJsonAsync($"/api/v1/workspaces/{name}/recipe/validation", new { hash, passed = true, reportBlob = (string?)null }, Ct)).EnsureSuccessStatusCode();

        for (var i = 0; i < 2; i++)
            (await admin.PostAsJsonAsync($"/api/v1/workspaces/{name}/recipe/confirmation", new { hash }, Ct)).EnsureSuccessStatusCode();

        await using var db = new NpgsqlConnection(stack.ConnectionString);
        var jobs = (await db.QueryAsync<string>(
            "SELECT payload::text FROM casebox.jobs WHERE org_id = @Org AND kind = 'env.build' AND payload->>'workspace' = @Name ORDER BY payload->>'repo'",
            new { Org = StackFixture.OrgA, Name = name })).Select(p => JsonDocument.Parse(p).RootElement).ToList();
        Assert.Equal(["github.com/acme/env-api", "github.com/acme/env-worker"], jobs.Select(j => j.GetProperty("repo").GetString()));
        Assert.All(jobs, j =>
        {
            Assert.Equal(hash, j.GetProperty("hash").GetString());
            Assert.Equal("golang:1.26", j.GetProperty("recipe").GetProperty("image").GetString());
        });
    }
}
