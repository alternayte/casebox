using System.Net;
using System.Net.Sockets;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Http;

namespace Casebox.Server.Tests.Infrastructure;

// A contract fake of the GitHub REST and GraphQL APIs, Jira Data Center and Azure DevOps Server: only the endpoints
// Casebox calls, with the fields it reads, from in-memory data a test sets.
public sealed class FakeServices : IAsyncDisposable
{
    private readonly WebApplication _app;

    public FakeServices()
    {
        Port = FreePort();
        var builder = WebApplication.CreateSlimBuilder();
        builder.WebHost.UseUrls($"http://127.0.0.1:{Port}");
        _app = builder.Build();
        Map(_app);
        _app.StartAsync().GetAwaiter().GetResult();
    }

    public int Port { get; }

    public Uri GitHubApi => new($"http://127.0.0.1:{Port}/github/");

    public Uri JiraUrl => new($"http://127.0.0.1:{Port}/jira/");

    public FakeAzureDevOps Ado { get; } = new();

    public string AdoCollection => $"http://127.0.0.1:{Port}{FakeAzureDevOps.Path}";

    public Dictionary<string, FakeRepo> Repos { get; } = new(StringComparer.OrdinalIgnoreCase);

    public List<JsonObject> JiraIssues { get; } = [];

    public List<JsonObject> JiraUsers { get; } = [];

    public const string GitHubToken = "ghp_fake_token_for_tests_only_000000000000";
    public const string JiraToken = "jira-fake-token";

    public FakeRepo Repo(string fullName) =>
        Repos.TryGetValue(fullName, out var r) ? r : Repos[fullName] = new FakeRepo(fullName);

    private void Map(WebApplication app)
    {
        app.Use(
            async (http, next) =>
            {
                var auth = http.Request.Headers.Authorization.ToString();
                var ok =
                    http.Request.Path.StartsWithSegments("/github/app")
                    || http.Request.Path.StartsWithSegments("/ado")
                    || auth
                        is $"Bearer {GitHubToken}"
                            or $"Bearer {JiraToken}"
                            or "Bearer ghs_installation_token";
                if (!ok)
                {
                    http.Response.StatusCode = 401;
                    return;
                }

                await next(http);
            }
        );

        Ado.Map(app);
        app.MapGet("/github/rate_limit", () => Results.Json(new { resources = new { } }));
        app.MapPost(
            "/github/app/installations/{id}/access_tokens",
            (long id) =>
                Results.Json(
                    new
                    {
                        token = "ghs_installation_token",
                        expires_at = DateTimeOffset.UtcNow.AddHours(1),
                    },
                    statusCode: 201
                )
        );
        app.MapGet(
            "/github/repos/{owner}/{name}",
            (string owner, string name) => Results.Json(new { default_branch = "main" })
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls",
            (string owner, string name, string? head) =>
                Results.Json(
                    Repo($"{owner}/{name}")
                        .Pulls.Values.Where(p => head is null || $"{owner}:{p.HeadRef}" == head)
                        .OrderByDescending(p => p.UpdatedAt)
                        .Select(p => p.Summary())
                )
        );
        // Opening a pull request, and the Git Data API that makes its branch.
        app.MapPost(
            "/github/repos/{owner}/{name}/pulls",
            async (string owner, string name, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                var repo = Repo($"{owner}/{name}");
                lock (repo.Pulls)
                {
                    var pull = new FakePull(repo.Pulls.Keys.DefaultIfEmpty(900).Max() + 1)
                    {
                        Title = body["title"]!.GetValue<string>(),
                        Body = body["body"]!.GetValue<string>(),
                        HeadRef = body["head"]!.GetValue<string>(),
                        AuthorLogin = "casebox-app[bot]",
                        UpdatedAt = DateTimeOffset.UtcNow,
                        CreatedAt = DateTimeOffset.UtcNow,
                    };
                    repo.Pulls[pull.Number] = pull;
                    return Results.Json(
                        new
                        {
                            number = pull.Number,
                            html_url = $"https://github.com/{owner}/{name}/pull/{pull.Number}",
                        },
                        statusCode: 201
                    );
                }
            }
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/git/ref/heads/{**branch}",
            (string owner, string name, string branch) =>
                Repo($"{owner}/{name}").Refs.TryGetValue(branch, out var sha)
                    ? Results.Json(new { @ref = $"refs/heads/{branch}", @object = new { sha } })
                    : Results.NotFound()
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/git/commits/{sha}",
            (string owner, string name, string sha) =>
                Results.Json(new { sha, tree = new { sha = $"tree-{sha}" } })
        );
        app.MapPost(
            "/github/repos/{owner}/{name}/git/trees",
            async (string owner, string name, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                var repo = Repo($"{owner}/{name}");
                lock (repo.Trees)
                {
                    repo.Trees.Add(body);
                    return Results.Json(
                        new { sha = $"tree-new-{repo.Trees.Count}" },
                        statusCode: 201
                    );
                }
            }
        );
        app.MapPost(
            "/github/repos/{owner}/{name}/git/commits",
            async (string owner, string name, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                return Results.Json(
                    new { sha = $"commit-{body["tree"]!.GetValue<string>()}" },
                    statusCode: 201
                );
            }
        );
        app.MapPost(
            "/github/repos/{owner}/{name}/git/refs",
            async (string owner, string name, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                var branch = body["ref"]!.GetValue<string>()["refs/heads/".Length..];
                var repo = Repo($"{owner}/{name}");
                lock (repo.Refs)
                {
                    if (!repo.Refs.TryAdd(branch, body["sha"]!.GetValue<string>()))
                        return Results.UnprocessableEntity();
                }
                return Results.Json(body, statusCode: 201);
            }
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls/{n:int}",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Pulls[n].Full())
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls/{n:int}/files",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Pulls[n].Files)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls/{n:int}/commits",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Pulls[n].Commits)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls/{n:int}/reviews",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Pulls[n].Reviews)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/pulls/{n:int}/comments",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Pulls[n].Comments)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/commits/{sha}/check-runs",
            (string owner, string name, string sha) =>
                Results.Json(
                    new
                    {
                        check_runs = Repo($"{owner}/{name}")
                            .Pulls.Values.SelectMany(p =>
                                p.Checks.Where(c => ((string?)c["head_sha"] ?? p.HeadSha) == sha)
                            ),
                    }
                )
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/commits/{sha}/pulls",
            (string owner, string name, string sha) =>
                Results.Json(
                    Repo($"{owner}/{name}")
                        .Pulls.Values.Where(p => p.Commits.Any(c => (string)c["sha"]! == sha))
                        .Select(p => new { number = p.Number })
                )
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/commits",
            (string owner, string name) =>
                Results.Json(Repo($"{owner}/{name}").DefaultBranchCommits)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/issues",
            (string owner, string name) => Results.Json(Repo($"{owner}/{name}").Issues)
        );
        app.MapGet(
            "/github/repos/{owner}/{name}/issues/{n:int}",
            (string owner, string name, int n) =>
                Results.Json(Repo($"{owner}/{name}").Issues.First(i => (int)i["number"]! == n))
        );
        app.MapPost(
            "/github/graphql",
            async (HttpContext http) =>
            {
                var body = await JsonSerializer.DeserializeAsync<JsonElement>(http.Request.Body);
                var v = body.GetProperty("variables");
                var repo = Repo(
                    $"{v.GetProperty("owner").GetString()}/{v.GetProperty("name").GetString()}"
                );
                var key = $"{v.GetProperty("oid").GetString()}:{v.GetProperty("path").GetString()}";
                var ranges = repo.Blame.TryGetValue(key, out var r) ? r : [];
                return Results.Json(
                    new
                    {
                        data = new
                        {
                            repository = new
                            {
                                @object = new
                                {
                                    blame = new
                                    {
                                        ranges = ranges.Select(x => new
                                        {
                                            startingLine = x.Start,
                                            endingLine = x.End,
                                            commit = new
                                            {
                                                associatedPullRequests = new
                                                {
                                                    nodes = new[] { new { number = x.Pr } },
                                                },
                                            },
                                        }),
                                    },
                                },
                            },
                        },
                    }
                );
            }
        );

        // Pull request comments, which harness CI posts and updates in place.
        app.MapGet(
            "/github/repos/{owner}/{name}/issues/{n:int}/comments",
            (string owner, string name, int n) =>
            {
                lock (Repo($"{owner}/{name}").IssueComments)
                    return Results.Json(
                        Repo($"{owner}/{name}")
                            .IssueComments.Where(c => c["number"]!.GetValue<int>() == n)
                            .ToList()
                    );
            }
        );
        app.MapPost(
            "/github/repos/{owner}/{name}/issues/{n:int}/comments",
            async (string owner, string name, int n, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                var comments = Repo($"{owner}/{name}").IssueComments;
                lock (comments)
                {
                    var comment = new JsonObject
                    {
                        ["id"] = comments.Count + 1000L,
                        ["number"] = n,
                        ["body"] = body["body"]!.GetValue<string>(),
                    };
                    comments.Add(comment);
                    return Results.Json(comment, statusCode: 201);
                }
            }
        );
        app.MapPatch(
            "/github/repos/{owner}/{name}/issues/comments/{id:long}",
            async (string owner, string name, long id, HttpRequest request) =>
            {
                var body = (await request.ReadFromJsonAsync<JsonObject>())!;
                var comments = Repo($"{owner}/{name}").IssueComments;
                lock (comments)
                {
                    var comment = comments.FirstOrDefault(c => c["id"]!.GetValue<long>() == id);
                    if (comment is null)
                        return Results.NotFound();
                    comment["body"] = body["body"]!.GetValue<string>();
                    comment["edits"] = (comment["edits"]?.GetValue<int>() ?? 0) + 1;
                    return Results.Json(comment);
                }
            }
        );

        app.MapGet("/jira/rest/api/2/myself", () => Results.Json(new { name = "casebox-bot" }));
        app.MapGet(
            "/jira/rest/api/2/search",
            (int startAt, int maxResults) =>
                Results.Json(
                    new
                    {
                        startAt,
                        maxResults,
                        total = JiraIssues.Count,
                        issues = JiraIssues.Skip(startAt).Take(maxResults),
                    }
                )
        );
        app.MapGet("/jira/rest/api/2/user/assignable/search", () => Results.Json(JiraUsers));
    }

    public ValueTask DisposeAsync() => _app.DisposeAsync();

    private static int FreePort()
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        return ((IPEndPoint)listener.LocalEndpoint).Port;
    }
}

public sealed record BlameRange(int Start, int End, int Pr);

public sealed class FakeRepo(string fullName)
{
    public string FullName { get; } = fullName;
    public Dictionary<int, FakePull> Pulls { get; } = [];
    public List<JsonObject> Issues { get; } = [];
    public List<JsonObject> DefaultBranchCommits { get; } = [];
    public Dictionary<string, List<BlameRange>> Blame { get; } = [];
    public List<JsonObject> IssueComments { get; } = [];
    public Dictionary<string, string> Refs { get; } = [];
    public List<JsonObject> Trees { get; } = [];
}

public sealed class FakePull(int number)
{
    public int Number { get; } = number;
    public string Title { get; set; } = "";
    public string? Body { get; set; }
    public string HeadRef { get; set; } = "";
    public string HeadSha { get; set; } = Sha();
    public string BaseSha { get; set; } = Sha();

    public static string Sha() =>
        (Guid.NewGuid().ToString("N") + Guid.NewGuid().ToString("N"))[..40];

    public string AuthorLogin { get; set; } = "";
    public DateTimeOffset CreatedAt { get; set; } = DateTimeOffset.UtcNow.AddDays(-2);
    public DateTimeOffset UpdatedAt { get; set; } = DateTimeOffset.UtcNow.AddDays(-1);
    public DateTimeOffset? MergedAt { get; set; }
    public List<JsonObject> Files { get; } = [];
    public List<JsonObject> Commits { get; } = [];
    public List<JsonObject> Reviews { get; } = [];
    public List<JsonObject> Comments { get; } = [];
    public List<JsonObject> Checks { get; } = [];

    public object Summary() => new { number = Number, updated_at = UpdatedAt };

    public object Full() =>
        new
        {
            number = Number,
            title = Title,
            body = Body,
            state = MergedAt is null ? "open" : "closed",
            draft = false,
            head = new { @ref = HeadRef, sha = HeadSha },
            @base = new { @ref = "main", sha = BaseSha },
            user = User(AuthorLogin),
            created_at = CreatedAt,
            updated_at = UpdatedAt,
            merged_at = MergedAt,
            merge_commit_sha = MergedAt is null ? null : HeadSha,
            labels = Array.Empty<object>(),
        };

    public static object User(string login) =>
        new { login, type = login.EndsWith("[bot]", StringComparison.Ordinal) ? "Bot" : "User" };
}
