using Dapper;
using Npgsql;

namespace Casebox.Server.Features.CodeHosts;

public static class CodeHostKinds
{
    public const string GitHub = "github";
    public const string AzureDevOps = "azure-devops";
}

// The code host that serves a repository, and for Azure DevOps the collection URL, such as
// http://ado.example.com:8080/tfs/DefaultCollection.
public sealed record RepoHost(string Host, string? Collection);

// One code host: it polls the pull requests, reviews, checks and reverts of the repositories it
// serves into the QueueBox poll inbox, as tokenized snapshots of one shape.
public interface ICodeHost
{
    string Kind { get; }

    Task<PollResult> PollAsync(string orgId, CancellationToken ct);
}

public sealed record PollResult(int PullRequests, int Issues, int Reverts, string? Error)
{
    public PollResult Add(PollResult other) =>
        new(
            PullRequests + other.PullRequests,
            Issues + other.Issues,
            Reverts + other.Reverts,
            Error ?? other.Error
        );
}

public static class RepoHosts
{
    // ado.example.com/tfs/defaultcollection: a collection URL as a repository name starts with it,
    // without scheme and port, lower case.
    public static string Prefix(string collection)
    {
        var uri = new Uri(collection);
        return $"{uri.Host}{uri.AbsolutePath.TrimEnd('/')}".ToLowerInvariant();
    }

    // A collection URL as Casebox stores it: http or https, no query, no trailing slash.
    public static string? NormalizeCollection(string? url)
    {
        if (
            !Uri.TryCreate(url?.Trim(), UriKind.Absolute, out var uri)
            || uri.Scheme is not ("https" or "http")
            || uri.AbsolutePath.Trim('/').Length == 0
            || !string.IsNullOrEmpty(uri.UserInfo)
        )
            return null;
        return uri.GetLeftPart(UriPartial.Path).TrimEnd('/');
    }

    // Azure DevOps when the name starts with a collection's prefix and one or two segments follow
    // (project and repository, or a repository named like its project); GitHub when the name is
    // host/owner/name and no collection claims it. The longest prefix wins.
    public static RepoHost? Resolve(string repo, IEnumerable<string> collections)
    {
        var match = collections
            .Select(c => (Collection: c, Prefix: Prefix(c)))
            .Where(c =>
                repo.StartsWith(c.Prefix + "/", StringComparison.Ordinal)
                && repo[(c.Prefix.Length + 1)..].Split('/').Length is 1 or 2
            )
            .OrderByDescending(c => c.Prefix.Length)
            .FirstOrDefault();
        if (match.Collection is not null)
            return new RepoHost(CodeHostKinds.AzureDevOps, match.Collection);
        return repo.Split('/').Length == 3 ? new RepoHost(CodeHostKinds.GitHub, null) : null;
    }

    // The project and repository of an Azure DevOps repository name, as they appear in URLs.
    public static (string Project, string Name) AdoPath(string repo, string collection)
    {
        var rest = repo[(Prefix(collection).Length + 1)..].Split('/');
        return rest.Length == 1 ? (rest[0], rest[0]) : (rest[0], rest[1]);
    }

    public static string CloneUrl(string repo, RepoHost host)
    {
        if (host.Host != CodeHostKinds.AzureDevOps || host.Collection is null)
            return $"https://{repo}.git";
        var (project, name) = AdoPath(repo, host.Collection);
        return $"{host.Collection}/{project}/_git/{name}";
    }

    // The repositories of an organisation that one code host serves.
    public static async Task<IReadOnlyList<(string Repo, string? Collection)>> ReposAsync(
        NpgsqlDataSource db,
        string orgId,
        string host,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        return (
            await connection.QueryAsync<(string, string?)>(
                new CommandDefinition(
                    """
                    SELECT r.repo, r.collection FROM casebox.repositories r
                    WHERE r.org_id = @Org AND r.host = @Host
                      AND EXISTS (SELECT 1 FROM casebox.workspaces w WHERE w.org_id = r.org_id AND w.repos @> to_jsonb(r.repo))
                    ORDER BY r.repo
                    """,
                    new { Org = orgId, Host = host },
                    cancellationToken: ct
                )
            )
        ).ToList();
    }
}

// Polls every code host of every organisation that connected it, every 5 minutes.
public sealed class CodeHostPoller(
    IEnumerable<ICodeHost> hosts,
    Integrations.IntegrationStore integrations,
    TimeProvider clock,
    ILogger<CodeHostPoller> logger
) : BackgroundService
{
    public static readonly TimeSpan Interval = TimeSpan.FromMinutes(5);

    // The first poll of a repository looks back half a year.
    public static readonly TimeSpan FirstWindow = TimeSpan.FromDays(183);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            foreach (var host in hosts)
            foreach (var org in await integrations.OrgsWithAsync(host.Kind, stoppingToken))
            {
                try
                {
                    await host.PollAsync(org, stoppingToken);
                }
                catch (Exception e) when (e is not OperationCanceledException)
                {
                    logger.LogWarning(
                        e,
                        "The {Host} poll of organisation {Org} failed; it runs again in 5 minutes.",
                        host.Kind,
                        org
                    );
                }
            }

            await Task.Delay(Interval, clock, stoppingToken);
        }
    }
}
