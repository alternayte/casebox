using System.Text;
using System.Text.Json.Nodes;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Http;

namespace Casebox.Server.Tests.Infrastructure;

// A contract fake of one Azure DevOps Server 2022 collection, REST API 7.0: only the endpoints
// Casebox calls, with the fields it reads, from in-memory data a test sets. Every call must name
// an api-version and carry the personal access token as Basic auth with an empty user name.
public sealed class FakeAzureDevOps
{
    public const string Token = "adofaketoken0000000000000000000000000000000000000000";
    public const string Path = "/ado/tfs/DefaultCollection";

    // The identity the token belongs to, for _apis/connectionData.
    public const string Self = "5a1f0000-0000-4000-8000-000000000001";

    private readonly object _lock = new();

    public Dictionary<string, FakeAdoRepo> Repos { get; } = new(StringComparer.OrdinalIgnoreCase);

    // By ID: what _apis/identities returns. An ID that is missing here is returned as null.
    public Dictionary<string, JsonObject> Identities { get; } =
        new(StringComparer.OrdinalIgnoreCase);

    // The token may read code but not identities, like a PAT with Code (Read) only.
    public bool RefuseIdentities { get; set; }

    public FakeAdoRepo Repo(string project, string name)
    {
        lock (_lock)
            return Repos.TryGetValue($"{project}/{name}", out var r)
                ? r
                : Repos[$"{project}/{name}"] = new FakeAdoRepo(project, name);
    }

    public static JsonObject Person(string id, string displayName, string? mail, string unique) =>
        new()
        {
            ["id"] = id,
            ["displayName"] = displayName,
            ["uniqueName"] = unique,
            ["descriptor"] = $"win.{Convert.ToBase64String(Encoding.UTF8.GetBytes(id))}",
            ["_mail"] = mail,
        };

    // The identity reference in a pull request: the fake keeps the mail aside for _apis/identities.
    public static JsonObject Ref(JsonObject person)
    {
        var copy = person.DeepClone().AsObject();
        copy.Remove("_mail");
        return copy;
    }

    public void AddIdentity(JsonObject person)
    {
        var mail = (string?)person["_mail"];
        Identities[(string)person["id"]!] = new JsonObject
        {
            ["id"] = (string)person["id"]!,
            ["descriptor"] = (string)person["descriptor"]!,
            ["providerDisplayName"] = (string)person["displayName"]!,
            ["isActive"] = true,
            ["isContainer"] = false,
            ["properties"] = new JsonObject
            {
                ["Account"] = new JsonObject
                {
                    ["$type"] = "System.String",
                    ["$value"] = (string)person["uniqueName"]!,
                },
                ["Mail"] = mail is null
                    ? null
                    : new JsonObject { ["$type"] = "System.String", ["$value"] = mail },
                ["SchemaClassName"] = new JsonObject
                {
                    ["$type"] = "System.String",
                    ["$value"] = "User",
                },
            },
        };
    }

    public void Map(WebApplication app)
    {
        var collection = app.MapGroup(Path);
        collection.AddEndpointFilter(
            async (context, next) =>
            {
                var http = context.HttpContext;
                var expected =
                    "Basic " + Convert.ToBase64String(Encoding.ASCII.GetBytes(":" + Token));
                if (http.Request.Headers.Authorization.ToString() != expected)
                    // Azure DevOps Server answers a bad token with its sign-in page.
                    return Results.Content("<html>Sign in</html>", "text/html", null, 203);
                var version = http.Request.Query["api-version"].ToString();
                if (version is not ("7.0" or "7.0-preview.1"))
                    return Results.BadRequest(new { message = "api-version is required" });
                return await next(context);
            }
        );

        collection.MapGet("/_apis/projects", () => Page(Repos.Values.Select(r => r.ProjectJson())));
        collection.MapGet(
            "/_apis/connectionData",
            () => Results.Json(new { authenticatedUser = new { id = Self } })
        );
        collection.MapGet(
            "/_apis/identities",
            (string identityIds) =>
            {
                if (RefuseIdentities)
                    return Results.StatusCode(401);
                var value = new JsonArray();
                foreach (var id in identityIds.Split(','))
                    value.Add(Identities.TryGetValue(id, out var i) ? i.DeepClone() : null);
                return Results.Json(new JsonObject { ["count"] = value.Count, ["value"] = value });
            }
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}",
            (string project, string name) =>
                Find(project, name) is { } r
                    ? Results.Json(
                        new
                        {
                            id = r.Id,
                            name = r.Name,
                            defaultBranch = "refs/heads/main",
                            project = r.ProjectJson(),
                        }
                    )
                    : Results.NotFound()
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullrequests",
            (string project, string name, HttpRequest request) =>
            {
                var r = Find(project, name);
                if (r is null)
                    return Results.NotFound();
                var status = request.Query["searchCriteria.status"].ToString();
                var pulls = r
                    .Pulls.Values.Select(p => p.Pr)
                    .Where(p =>
                        status == "all"
                        || (string)p["status"]! == (status is "" ? "active" : status)
                    )
                    .OrderByDescending(p => (int)p["pullRequestId"]!)
                    .Select(p => (JsonNode)p.DeepClone());
                return Paged(request, pulls, "");
            }
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullRequests/{id:int}/iterations",
            (string project, string name, int id) => Page(Pull(project, name, id).Iterations)
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullRequests/{id:int}/iterations/{iteration:int}/changes",
            (string project, string name, int id, int iteration) =>
                Results.Json(
                    new JsonObject
                    {
                        ["changeEntries"] = new JsonArray([
                            .. Pull(project, name, id)
                                .Files.Select(f =>
                                    (JsonNode)
                                        new JsonObject
                                        {
                                            ["changeType"] = "edit",
                                            ["item"] = new JsonObject { ["path"] = "/" + f },
                                        }
                                ),
                        ]),
                    }
                )
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullRequests/{id:int}/commits",
            (string project, string name, int id) =>
                Page(Pull(project, name, id).Commits.AsEnumerable().Reverse())
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullRequests/{id:int}/threads",
            (string project, string name, int id) => Page(Pull(project, name, id).Threads)
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/pullRequests/{id:int}/statuses",
            (string project, string name, int id) => Page(Pull(project, name, id).Statuses)
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/commits",
            (string project, string name, HttpRequest request) =>
            {
                var r = Find(project, name);
                if (r is null)
                    return Results.NotFound();
                if (request.Query["searchCriteria.itemVersion.version"] != "main")
                    return Results.NotFound();
                // Lists cut a long message short.
                var commits = r
                    .DefaultBranchCommits.AsEnumerable()
                    .Reverse()
                    .Select(c =>
                    {
                        var copy = c.DeepClone().AsObject();
                        var comment = (string)copy["comment"]!;
                        if (comment.Length > 80)
                        {
                            copy["comment"] = comment[..80];
                            copy["commentTruncated"] = true;
                        }

                        return (JsonNode)copy;
                    });
                return Paged(request, commits, "searchCriteria.");
            }
        );
        collection.MapGet(
            "/{project}/_apis/git/repositories/{name}/commits/{sha}",
            (string project, string name, string sha) =>
                Find(project, name)
                    ?.DefaultBranchCommits.Concat(
                        Find(project, name)!.Pulls.Values.SelectMany(p => p.Commits)
                    )
                    .FirstOrDefault(c => (string)c["commitId"]! == sha)
                    is { } commit
                    ? Results.Json(commit)
                    : Results.NotFound()
        );
        collection.MapGet(
            "/{project}/_apis/policy/evaluations",
            (string project, string artifactId) =>
            {
                // vstfs:///CodeReview/CodeReviewId/{projectId}/{pullRequestId}
                var parts = artifactId.Split('/');
                var repo = Repos.Values.FirstOrDefault(r =>
                    r.Project.Equals(project, StringComparison.OrdinalIgnoreCase)
                    && r.ProjectId == parts[^2]
                );
                var id = int.Parse(parts[^1], System.Globalization.CultureInfo.InvariantCulture);
                return repo is null || !repo.Pulls.TryGetValue(id, out var pull)
                    ? Results.BadRequest()
                    : Page(pull.Evaluations);
            }
        );
    }

    private FakeAdoRepo? Find(string project, string name) =>
        Repos.TryGetValue($"{project}/{name}", out var r) ? r : null;

    private FakeAdoPull Pull(string project, string name, int id) => Find(project, name)!.Pulls[id];

    private static IResult Page(IEnumerable<JsonNode?> items)
    {
        var value = new JsonArray([.. items.Select(i => i?.DeepClone())]);
        return Results.Json(new JsonObject { ["count"] = value.Count, ["value"] = value });
    }

    private static IResult Paged(HttpRequest request, IEnumerable<JsonNode> items, string prefix)
    {
        var top = int.TryParse(request.Query[$"{prefix}$top"], out var t) ? t : 101;
        var skip = int.TryParse(request.Query[$"{prefix}$skip"], out var s) ? s : 0;
        return Page(items.Skip(skip).Take(top));
    }
}

public sealed class FakeAdoRepo(string project, string name)
{
    public string Project { get; } = project;
    public string Name { get; } = name;
    public string Id { get; } = Guid.NewGuid().ToString();
    public string ProjectId { get; } = Guid.NewGuid().ToString();
    public Dictionary<int, FakeAdoPull> Pulls { get; } = [];

    // Oldest first; the fake answers newest first, as the server does.
    public List<JsonObject> DefaultBranchCommits { get; } = [];

    public JsonObject ProjectJson() => new() { ["id"] = ProjectId, ["name"] = Project };
}

public sealed class FakeAdoPull(JsonObject pr)
{
    public JsonObject Pr { get; } = pr;
    public List<JsonObject> Iterations { get; } = [];

    // Oldest first; the fake answers newest first, as the server does.
    public List<JsonObject> Commits { get; } = [];
    public List<JsonObject> Threads { get; } = [];
    public List<JsonObject> Statuses { get; } = [];
    public List<JsonObject> Evaluations { get; } = [];
    public List<string> Files { get; } = [];
}
