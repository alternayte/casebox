using System.Collections.Immutable;
using System.Text.RegularExpressions;
using Deedbox;

namespace Casebox.Server.Features.Workspaces;

public static class WorkspaceEvents
{
    public sealed record Created(string Name);

    // Host and Collection say which code host serves the repository; events from before
    // Azure DevOps have neither and are GitHub repositories.
    public sealed record RepoAdded(string Repo, string? Host = null, string? Collection = null);

    public sealed record RepoRemoved(string Repo);
}

public sealed record Workspace(bool Exists, string Name, ImmutableSortedSet<string> Repos)
    : IState<Workspace>
{
    public static Workspace Initial { get; } = new(false, "", ImmutableSortedSet<string>.Empty);

    public static Workspace Evolve(Workspace state, object @event) =>
        @event switch
        {
            WorkspaceEvents.Created e => state with { Exists = true, Name = e.Name },
            WorkspaceEvents.RepoAdded e => state with { Repos = state.Repos.Add(e.Repo) },
            WorkspaceEvents.RepoRemoved e => state with { Repos = state.Repos.Remove(e.Repo) },
            _ => state,
        };

    public static string StreamIdFor(string name) => $"workspace:{name}";
}

public static partial class WorkspaceDecider
{
    [GeneratedRegex("^[a-z0-9][a-z0-9-]{0,62}$")]
    private static partial Regex NamePattern();

    // github.com/owner/name, or ado.example.com/tfs/defaultcollection/project/name: the host and
    // the path of the remote without /_git, lower case.
    [GeneratedRegex("^[a-z0-9.-]+(/[a-z0-9_.%-]+){2,8}$")]
    private static partial Regex RepoPattern();

    public static string NormalizeRepo(string repo)
    {
        var r = repo.Trim().ToLowerInvariant();
        foreach (var prefix in new[] { "https://", "http://" })
            if (r.StartsWith(prefix, StringComparison.Ordinal))
                r = r[prefix.Length..];
        if (r.EndsWith(".git", StringComparison.Ordinal))
            r = r[..^4];
        r = r.TrimEnd('/').Replace("/_git/", "/", StringComparison.Ordinal);
        if (!RepoPattern().IsMatch(r))
            throw new DomainException($"'{repo}' is not a repository like github.com/owner/name.");
        return r;
    }

    public static IEnumerable<object> Create(Workspace workspace, string name)
    {
        if (!NamePattern().IsMatch(name))
            throw new DomainException(
                "A workspace name is 1 to 63 lower-case letters, digits and dashes, starting with a letter or digit."
            );
        if (workspace.Exists)
            throw new ConflictException($"The workspace '{name}' already exists.");
        return [new WorkspaceEvents.Created(name)];
    }

    // collections: the Azure DevOps collection URLs Casebox knows, so the repository's code host is
    // found from its name and stored with it.
    public static IEnumerable<object> AddRepo(
        Workspace workspace,
        string repo,
        IReadOnlyCollection<string>? collections = null
    )
    {
        Require(workspace);
        var normalized = NormalizeRepo(repo);
        var host =
            CodeHosts.RepoHosts.Resolve(normalized, collections ?? [])
            ?? throw new DomainException(
                $"'{normalized}' is not a GitHub repository and starts with no Azure DevOps collection Casebox knows; name its collection URL, such as https://ado.example.com/tfs/DefaultCollection.",
                Cbx.UnknownCodeHost
            );
        return workspace.Repos.Contains(normalized)
            ? []
            : [new WorkspaceEvents.RepoAdded(normalized, host.Host, host.Collection)];
    }

    public static IEnumerable<object> RemoveRepo(Workspace workspace, string repo)
    {
        Require(workspace);
        var normalized = NormalizeRepo(repo);
        return workspace.Repos.Contains(normalized)
            ? [new WorkspaceEvents.RepoRemoved(normalized)]
            : [];
    }

    private static void Require(Workspace workspace)
    {
        if (!workspace.Exists)
            throw new NotFoundException("The workspace does not exist.", Cbx.NoWorkspace);
    }
}
