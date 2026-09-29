using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Jobs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Workspaces;

public static class WorkspaceEndpoints
{
    public sealed record CreateWorkspace(string Name);

    public sealed record RepoBody(string Repo);

    public sealed record ProposeRecipe(JsonElement Recipe);

    public sealed record RecipeValidation(string Hash, bool Passed, string? ReportBlob);

    public sealed record RecipeConfirmation(string Hash);

    public sealed record WorkspaceSummary(
        string Name,
        IReadOnlyList<string> Repos,
        RecipeStatus RecipeStatus
    );

    public sealed record WorkspaceView(
        string Name,
        IReadOnlyList<string> Repos,
        RecipeStatus RecipeStatus,
        string? RecipeHash,
        bool CanMine
    );

    public sealed record HarnessBody(IReadOnlyList<string>? Globs, string? Shared);

    public static void MapWorkspaces(this RouteGroupBuilder api)
    {
        var workspaces = api.MapGroup("/workspaces").WithTags("Workspaces");

        workspaces
            .MapGet(
                "/",
                async (HttpContext http, NpgsqlDataSource db) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    var rows = await connection.QueryAsync<(
                        string Name,
                        string Repos,
                        string RecipeStatus
                    )>(
                        new CommandDefinition(
                            "SELECT name, repos::text, recipe_status FROM casebox.workspaces WHERE org_id = @Org ORDER BY name",
                            new { Org = http.User.OrgId() },
                            cancellationToken: http.RequestAborted
                        )
                    );
                    return Results.Ok(
                        rows.Select(r => new WorkspaceSummary(
                                r.Name,
                                JsonSerializer.Deserialize<List<string>>(r.Repos)!,
                                Enum.Parse<RecipeStatus>(
                                    r.RecipeStatus.Replace("_", "", StringComparison.Ordinal),
                                    ignoreCase: true
                                )
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
                (string name, RepoBody body, IEventStore store) =>
                    Execute(store, name, w => WorkspaceDecider.AddRepo(w, body.Repo ?? ""))
            )
            .RequireAuthorization(Policies.Admin);

        workspaces
            .MapDelete(
                "/{name}/repos/{**repo}",
                (string name, string repo, IEventStore store) =>
                    Execute(store, name, w => WorkspaceDecider.RemoveRepo(w, repo))
            )
            .RequireAuthorization(Policies.Admin);

        workspaces
            .MapPut(
                "/{name}/harness",
                (string name, HarnessBody body, IEventStore store) =>
                    Execute(
                        store,
                        name,
                        w => WorkspaceDecider.ConfigureHarness(w, body.Globs ?? [], body.Shared)
                    )
            )
            .RequireAuthorization(Policies.Member);

        workspaces
            .MapPut(
                "/{name}/recipe",
                (string name, ProposeRecipe body, IEventStore store) =>
                {
                    var recipe = body.Recipe.GetRawText();
                    var hash = Convert.ToHexStringLower(
                        SHA256.HashData(Encoding.UTF8.GetBytes(recipe))
                    );
                    return Execute(
                        store,
                        name,
                        w => WorkspaceDecider.ProposeRecipe(w, recipe, hash)
                    );
                }
            )
            .RequireAuthorization(Policies.Member);

        workspaces
            .MapPost(
                "/{name}/recipe/validation",
                (string name, RecipeValidation body, IEventStore store) =>
                    Execute(
                        store,
                        name,
                        w =>
                            WorkspaceDecider.RecordValidation(
                                w,
                                body.Hash,
                                body.Passed,
                                body.ReportBlob
                            )
                    )
            )
            .RequireAuthorization(Policies.Member);

        // A confirmed recipe gets its environment prepared on the workers, once per repository.
        workspaces
            .MapPost(
                "/{name}/recipe/confirmation",
                async (
                    string name,
                    RecipeConfirmation body,
                    HttpContext http,
                    IEventStore store,
                    NpgsqlDataSource db,
                    JobQueue jobs
                ) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    await using var transaction = await connection.BeginTransactionAsync(
                        http.RequestAborted
                    );
                    var result = await store
                        .UseTransaction(transaction)
                        .Execute<Workspace>(
                            Workspace.StreamIdFor(name),
                            w => WorkspaceDecider.ConfirmRecipe(w, body.Hash),
                            http.RequestAborted
                        );
                    var w = result.State;
                    foreach (var repo in w.Repos)
                        await jobs.EnqueueAsync(
                            transaction,
                            http.User.OrgId(),
                            EnvBuild.Kind,
                            $"{EnvBuild.Kind}:{w.Name}:{repo}:{w.RecipeHash}",
                            new
                            {
                                workspace = w.Name,
                                repo,
                                recipe = JsonDocument.Parse(w.Recipe!).RootElement,
                                hash = w.RecipeHash,
                            },
                            3,
                            http.RequestAborted
                        );
                    await transaction.CommitAsync(http.RequestAborted);
                    return Results.Ok(View(w));
                }
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

    private static WorkspaceView View(Workspace w) =>
        new(w.Name, w.Repos.ToList(), w.RecipeStatus, w.RecipeHash, w.CanMine);
}

// env.build: a worker prepares a confirmed recipe's environment in its own cache, so case runs start
// from it (docs/specs/sandboxes.md). The job's result is all the server keeps.
public sealed class EnvBuild : IJobResultHandler
{
    public const string Kind = "env.build";

    string IJobResultHandler.Kind => Kind;

    public Task HandleAsync(JobResult result, CancellationToken ct) => Task.CompletedTask;
}
