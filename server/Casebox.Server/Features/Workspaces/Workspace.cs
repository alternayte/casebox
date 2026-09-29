using System.Collections.Immutable;
using System.Text.RegularExpressions;
using Deedbox;

namespace Casebox.Server.Features.Workspaces;

public static class WorkspaceEvents
{
    public sealed record Created(string Name);

    public sealed record RepoAdded(string Repo);

    public sealed record RepoRemoved(string Repo);

    // The recipe is the environment block of casebox.yml as JSON; its hash identifies it.
    public sealed record RecipeProposed(string Recipe, string Hash);

    public sealed record RecipeValidated(string Hash, bool Passed, string? ReportBlob);

    public sealed record RecipeConfirmed(string Hash);
}

public enum RecipeStatus { None, Proposed, Validated, ValidationFailed, Confirmed }

public sealed record Workspace(
    bool Exists,
    string Name,
    ImmutableSortedSet<string> Repos,
    string? RecipeHash,
    RecipeStatus RecipeStatus) : IState<Workspace>
{
    public static Workspace Initial { get; } = new(false, "", ImmutableSortedSet<string>.Empty, null, RecipeStatus.None);

    public static Workspace Evolve(Workspace state, object @event) => @event switch
    {
        WorkspaceEvents.Created e => state with { Exists = true, Name = e.Name },
        WorkspaceEvents.RepoAdded e => state with { Repos = state.Repos.Add(e.Repo) },
        WorkspaceEvents.RepoRemoved e => state with { Repos = state.Repos.Remove(e.Repo) },
        WorkspaceEvents.RecipeProposed e => state with { RecipeHash = e.Hash, RecipeStatus = RecipeStatus.Proposed },
        WorkspaceEvents.RecipeValidated e => state with { RecipeStatus = e.Passed ? RecipeStatus.Validated : RecipeStatus.ValidationFailed },
        WorkspaceEvents.RecipeConfirmed => state with { RecipeStatus = RecipeStatus.Confirmed },
        _ => state,
    };

    // Cases are mined only in a workspace with a confirmed recipe.
    public bool CanMine => RecipeStatus == RecipeStatus.Confirmed;

    public static string StreamIdFor(string name) => $"workspace:{name}";
}

public static partial class WorkspaceDecider
{
    [GeneratedRegex("^[a-z0-9][a-z0-9-]{0,62}$")]
    private static partial Regex NamePattern();

    // github.com/owner/name: the host, the owner and the repository, lower case.
    [GeneratedRegex("^[a-z0-9.-]+/[a-z0-9_.-]+/[a-z0-9_.-]+$")]
    private static partial Regex RepoPattern();

    public static string NormalizeRepo(string repo)
    {
        var r = repo.Trim().ToLowerInvariant();
        foreach (var prefix in new[] { "https://", "http://" })
            if (r.StartsWith(prefix, StringComparison.Ordinal)) r = r[prefix.Length..];
        if (r.EndsWith(".git", StringComparison.Ordinal)) r = r[..^4];
        r = r.TrimEnd('/');
        if (!RepoPattern().IsMatch(r)) throw new DomainException($"'{repo}' is not a repository like github.com/owner/name.");
        return r;
    }

    public static IEnumerable<object> Create(Workspace workspace, string name)
    {
        if (!NamePattern().IsMatch(name))
            throw new DomainException("A workspace name is 1 to 63 lower-case letters, digits and dashes, starting with a letter or digit.");
        if (workspace.Exists) throw new ConflictException($"The workspace '{name}' already exists.");
        return [new WorkspaceEvents.Created(name)];
    }

    public static IEnumerable<object> AddRepo(Workspace workspace, string repo)
    {
        Require(workspace);
        var normalized = NormalizeRepo(repo);
        return workspace.Repos.Contains(normalized) ? [] : [new WorkspaceEvents.RepoAdded(normalized)];
    }

    public static IEnumerable<object> RemoveRepo(Workspace workspace, string repo)
    {
        Require(workspace);
        var normalized = NormalizeRepo(repo);
        return workspace.Repos.Contains(normalized) ? [new WorkspaceEvents.RepoRemoved(normalized)] : [];
    }

    public static IEnumerable<object> ProposeRecipe(Workspace workspace, string recipe, string hash)
    {
        Require(workspace);
        if (string.IsNullOrWhiteSpace(recipe)) throw new DomainException("The recipe is empty.");
        return workspace.RecipeHash == hash && workspace.RecipeStatus != RecipeStatus.None
            ? []
            : [new WorkspaceEvents.RecipeProposed(recipe, hash)];
    }

    public static IEnumerable<object> RecordValidation(Workspace workspace, string hash, bool passed, string? reportBlob)
    {
        Require(workspace);
        if (workspace.RecipeHash != hash)
            throw new ConflictException("The validation is for a recipe that is no longer the proposed one.");
        if (workspace.RecipeStatus == RecipeStatus.Confirmed)
            throw new ConflictException("The recipe is already confirmed.");
        return [new WorkspaceEvents.RecipeValidated(hash, passed, reportBlob)];
    }

    public static IEnumerable<object> ConfirmRecipe(Workspace workspace, string hash)
    {
        Require(workspace);
        if (workspace.RecipeHash != hash)
            throw new ConflictException("The confirmation is for a recipe that is no longer the proposed one.");
        return workspace.RecipeStatus switch
        {
            RecipeStatus.Confirmed => [],
            RecipeStatus.Validated => [new WorkspaceEvents.RecipeConfirmed(hash)],
            _ => throw new DomainException("Only a recipe whose check passed can be confirmed. Run casebox env check first."),
        };
    }

    private static void Require(Workspace workspace)
    {
        if (!workspace.Exists) throw new NotFoundException("The workspace does not exist.");
    }
}
