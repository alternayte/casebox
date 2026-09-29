using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Integrations;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Workspaces;

public static class WorkspaceEndpoints
{
    public sealed record CreateWorkspace(string Name);

    // Collection: the Azure DevOps collection URL of the repository, when Casebox has no token for
    // that collection yet.
    public sealed record RepoBody(string Repo, string? Collection = null);

    public sealed record WorkspaceSummary(string Name, IReadOnlyList<string> Repos);

    public sealed record WorkspaceView(string Name, IReadOnlyList<string> Repos);

    public static void MapWorkspaces(this RouteGroupBuilder api)
    {
        var workspaces = api.MapGroup("/workspaces").WithTags("Workspaces");

        workspaces
            .MapGet(
                "/",
                async (HttpContext http, NpgsqlDataSource db) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    var rows = await connection.QueryAsync<(string Name, string Repos)>(
                        new CommandDefinition(
                            "SELECT name, repos::text FROM casebox.workspaces WHERE org_id = @Org ORDER BY name",
                            new { Org = http.User.OrgId() },
                            cancellationToken: http.RequestAborted
                        )
                    );
                    return Results.Ok(
                        rows.Select(r => new WorkspaceSummary(
                                r.Name,
                                JsonSerializer.Deserialize<List<string>>(r.Repos)!
                            ))
                            .ToList()
                    );
                }
            )
            .RequireAuthorization(Policies.Viewer);

        workspaces
            .MapGet(
                "/{name}",
                async (string name, IEventStore store) =>
                {
                    var (state, _) = await store.Load<Workspace>(Workspace.StreamIdFor(name));
                    return state.Exists ? Results.Ok(View(state)) : Results.NotFound();
                }
            )
            .RequireAuthorization(Policies.Viewer);

        workspaces
            .MapPost(
                "/",
                async (CreateWorkspace body, IEventStore store) =>
                {
                    var result = await store.Execute<Workspace>(
                        Workspace.StreamIdFor(body.Name ?? ""),
                        w => WorkspaceDecider.Create(w, body.Name ?? "")
                    );
                    return Results.Created(
                        $"/api/v1/workspaces/{result.State.Name}",
                        View(result.State)
                    );
                }
            )
            .RequireAuthorization(Policies.Admin);

        workspaces
            .MapPost(
                "/{name}/repos",
                async (
                    string name,
                    RepoBody body,
                    HttpContext http,
                    IEventStore store,
                    IntegrationStore integrations
                ) =>
                {
                    var collections =
                        (
                            await integrations.ConfigAsync<AzureDevOpsSettings>(
                                http.User.OrgId(),
                                CodeHosts.CodeHostKinds.AzureDevOps,
                                http.RequestAborted
                            )
                        )?.Collections.ToList() ?? [];
                    if (body.Collection is not null)
                    {
                        var collection =
                            CodeHosts.RepoHosts.NormalizeCollection(body.Collection)
                            ?? throw new DomainException(
                                "The collection must be an http or https URL with its path, such as https://ado.example.com/tfs/DefaultCollection."
                            );
                        if (
                            !WorkspaceDecider
                                .NormalizeRepo(body.Repo ?? "")
                                .StartsWith(
                                    CodeHosts.RepoHosts.Prefix(collection) + "/",
                                    StringComparison.Ordinal
                                )
                        )
                            throw new DomainException(
                                $"The repository '{body.Repo}' is not in the collection {collection}.",
                                Cbx.UnknownCodeHost
                            );
                        collections = [collection];
                    }

                    return await Execute(
                        store,
                        name,
                        w => WorkspaceDecider.AddRepo(w, body.Repo ?? "", collections)
                    );
                }
            )
            .RequireAuthorization(Policies.Admin);

        workspaces
            .MapDelete(
                "/{name}/repos/{**repo}",
                (string name, string repo, IEventStore store) =>
                    Execute(store, name, w => WorkspaceDecider.RemoveRepo(w, repo))
            )
            .RequireAuthorization(Policies.Admin);
    }

    private static async Task<IResult> Execute(
        IEventStore store,
        string name,
        Func<Workspace, IEnumerable<object>> decide
    )
    {
        var result = await store.Execute<Workspace>(Workspace.StreamIdFor(name), decide);
        return Results.Ok(View(result.State));
    }

    private static WorkspaceView View(Workspace w) => new(w.Name, w.Repos.ToList());
}
