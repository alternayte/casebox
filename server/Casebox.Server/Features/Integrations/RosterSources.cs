using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text.Json;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Privacy;
using Dapper;
using Npgsql;

namespace Casebox.Server.Features.Integrations;

// Who is who on GitHub: each repository's recent commits pair a login with a git email and name,
// the identities capture marks. Read into memory for the roster; never stored.
public sealed class GitHubRosterSource(GitHubClients clients, NpgsqlDataSource db) : IRosterSource
{
    private const int CommitPages = 3;

    public async Task<IReadOnlyList<RosterPerson>> PeopleAsync(string orgId, CancellationToken ct)
    {
        var client = await clients.ForOrgAsync(orgId, ct);
        if (client is null)
            return [];
        await using var connection = await db.OpenConnectionAsync(ct);
        var repos = await connection.QueryAsync<string>(
            new CommandDefinition(
                "SELECT DISTINCT jsonb_array_elements_text(repos) FROM casebox.workspaces WHERE org_id = @Org",
                new { Org = orgId },
                cancellationToken: ct
            )
        );

        var people = new Dictionary<string, HashSet<string>>(StringComparer.Ordinal);
        foreach (var repo in repos)
        {
            var (owner, name) = GitHubReader.Split(repo);
            foreach (
                var commit in await client.ListAsync(
                    $"repos/{owner}/{name}/commits",
                    null,
                    CommitPages,
                    ct
                )
            )
            {
                foreach (
                    var (user, person) in new[] { ("author", "author"), ("committer", "committer") }
                )
                {
                    if (
                        !commit.TryGetProperty(user, out var u)
                        || u.ValueKind != JsonValueKind.Object
                    )
                        continue;
                    var login = u.GetProperty("login").GetString()!;
                    if (login.EndsWith("[bot]", StringComparison.Ordinal))
                        continue;
                    var ids = people.TryGetValue(login, out var set)
                        ? set
                        : people[login] = new HashSet<string>(StringComparer.Ordinal);
                    var git = commit.GetProperty("commit").GetProperty(person);
                    if (
                        git.TryGetProperty("email", out var email)
                        && email.GetString() is { Length: > 0 } e
                        && !e.EndsWith(
                            "@users.noreply.github.com",
                            StringComparison.OrdinalIgnoreCase
                        )
                    )
                        ids.Add($"email:{e}");
                    if (
                        git.TryGetProperty("name", out var n) && n.GetString() is { Length: > 2 } nm
                    )
                        ids.Add($"name:{nm}");
                }
            }
        }

        return people.Select(p => new RosterPerson($"github:{p.Key}", [.. p.Value])).ToList();
    }
}

// Who is who in Jira: the assignable users of each configured project, with their email when
// Jira shows it, which joins them to their GitHub identity.
public sealed class JiraRosterSource(IntegrationStore integrations, IHttpClientFactory http)
    : IRosterSource
{
    public async Task<IReadOnlyList<RosterPerson>> PeopleAsync(string orgId, CancellationToken ct)
    {
        var stored = await integrations.GetAsync<JiraSettings, JiraSecret>(orgId, "jira", ct);
        if (stored is not { } jira)
            return [];
        var baseUri = new Uri(jira.Config.Url);
        var people = new List<RosterPerson>();
        foreach (var project in jira.Config.Projects)
        {
            using var request = new HttpRequestMessage(
                HttpMethod.Get,
                new Uri(
                    baseUri,
                    $"rest/api/2/user/assignable/search?project={Uri.EscapeDataString(project)}&maxResults=1000"
                )
            );
            request.Headers.Authorization = new AuthenticationHeaderValue(
                "Bearer",
                jira.Secret.Token
            );
            using var response = await http.CreateClient("jira").SendAsync(request, ct);
            response.EnsureSuccessStatusCode();
            foreach (
                var user in (
                    await response.Content.ReadFromJsonAsync<JsonElement>(ct)
                ).EnumerateArray()
            )
            {
                var account = user.TryGetProperty("name", out var n)
                    ? n.GetString()
                    : user.GetProperty("key").GetString();
                if (string.IsNullOrEmpty(account))
                    continue;
                var ids = new List<string>();
                if (
                    user.TryGetProperty("emailAddress", out var e)
                    && e.GetString() is { Length: > 0 } email
                )
                    ids.Add($"email:{email}");
                if (
                    user.TryGetProperty("displayName", out var d)
                    && d.GetString() is { Length: > 2 } display
                )
                    ids.Add($"name:{display}");
                people.Add(new RosterPerson($"jira:{account}", ids));
            }
        }

        return people;
    }
}
