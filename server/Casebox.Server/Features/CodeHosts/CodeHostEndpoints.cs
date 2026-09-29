using System.Net;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.AzureDevOps;
using Casebox.Server.Features.Integrations;
using Dapper;
using Npgsql;

namespace Casebox.Server.Features.CodeHosts;

public static class CodeHostEndpoints
{
    // Reachable and IdentitiesReadable are null when nothing was checked: no token is stored.
    public sealed record HostStatus(
        string Repo,
        string Host,
        string? Collection,
        bool Connected,
        bool? Reachable,
        bool? IdentitiesReadable,
        string? Error
    );

    public sealed record WorkerRepo(string Repo, string Host, string CloneUrl);

    public static void MapCodeHosts(this RouteGroupBuilder api)
    {
        // What casebox doctor shows for the repository it runs in: its code host, and whether the
        // server can read it with the token it holds.
        api.MapGet(
                "/repos/host",
                async (
                    string repo,
                    HttpContext http,
                    NpgsqlDataSource db,
                    AdoClients ado,
                    GitHubClients github
                ) =>
                {
                    var ct = http.RequestAborted;
                    var org = http.User.OrgId();
                    var normalized = Workspaces.WorkspaceDecider.NormalizeRepo(repo);
                    var host = await HostOfAsync(db, org, normalized, ct);
                    if (host is null)
                        throw new NotFoundException(
                            "The repository is in no workspace; run casebox init in it."
                        );
                    if (host.Host == CodeHostKinds.GitHub)
                    {
                        var client = await github.ForOrgAsync(org, ct);
                        if (client is null)
                            return Results.Ok(
                                new HostStatus(normalized, host.Host, null, false, null, null, null)
                            );
                        try
                        {
                            await client.GetAsync("rate_limit", ct);
                            return Results.Ok(
                                new HostStatus(normalized, host.Host, null, true, true, null, null)
                            );
                        }
                        catch (HttpRequestException e)
                        {
                            return Results.Ok(
                                new HostStatus(
                                    normalized,
                                    host.Host,
                                    null,
                                    true,
                                    false,
                                    null,
                                    e.Message
                                )
                            );
                        }
                    }

                    var clients = await ado.ForOrgAsync(org, ct);
                    if (!clients.TryGetValue(host.Collection!, out var collection))
                        return Results.Ok(
                            new HostStatus(
                                normalized,
                                host.Host,
                                host.Collection,
                                false,
                                null,
                                null,
                                null
                            )
                        );
                    try
                    {
                        await collection.GetAsync(
                            $"{AdoPoller.Repo(normalized, host.Collection!).Api}",
                            ct
                        );
                        var me = await collection.GetAsync("_apis/connectionData", ct);
                        var people = new AdoPeople(collection);
                        if (
                            me.TryGetProperty("authenticatedUser", out var user)
                            && AdoPeople.Text(user, "id") is { } id
                        )
                            await people.LoadAsync([id], ct);
                        return Results.Ok(
                            new HostStatus(
                                normalized,
                                host.Host,
                                host.Collection,
                                true,
                                true,
                                !people.Refused,
                                people.Refused
                                    ? "The token cannot read identities; give it Identity (Read), or people stay unmapped."
                                    : null
                            )
                        );
                    }
                    catch (HttpRequestException e)
                    {
                        return Results.Ok(
                            new HostStatus(
                                normalized,
                                host.Host,
                                host.Collection,
                                true,
                                false,
                                null,
                                e.StatusCode is HttpStatusCode.NotFound
                                    ? $"The collection has no repository {normalized}."
                                    : e.Message
                            )
                        );
                    }
                }
            )
            .WithTags("Integrations")
            .RequireAuthorization(Policies.Viewer);
    }

    // Where a worker clones each workspace repository from. The worker brings its own token.
    public static void MapCodeHostWorker(this RouteGroupBuilder worker)
    {
        worker
            .MapGet(
                "/repos",
                async (HttpContext http, NpgsqlDataSource db) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    var rows = await connection.QueryAsync<(
                        string Repo,
                        string Host,
                        string? Collection
                    )>(
                        new CommandDefinition(
                            """
                            SELECT r.repo, r.host, r.collection FROM casebox.repositories r
                            WHERE r.org_id = @Org AND EXISTS (SELECT 1 FROM casebox.workspaces w WHERE w.org_id = r.org_id AND w.repos @> to_jsonb(r.repo))
                            ORDER BY r.repo
                            """,
                            new { Org = http.User.OrgId() },
                            cancellationToken: http.RequestAborted
                        )
                    );
                    return Results.Ok(
                        rows.Select(r => new WorkerRepo(
                                r.Repo,
                                r.Host,
                                RepoHosts.CloneUrl(r.Repo, new RepoHost(r.Host, r.Collection))
                            ))
                            .ToList()
                    );
                }
            )
            .WithTags("Worker")
            .RequireAuthorization(Policies.Worker);
    }

    public static async Task<RepoHost?> HostOfAsync(
        NpgsqlDataSource db,
        string orgId,
        string repo,
        CancellationToken ct
    )
    {
        await using var connection = await db.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleOrDefaultAsync<(string Host, string? Collection)?>(
            new CommandDefinition(
                "SELECT host, collection FROM casebox.repositories WHERE org_id = @Org AND repo = @Repo",
                new { Org = orgId, Repo = repo },
                cancellationToken: ct
            )
        );
        return row is { } r ? new RepoHost(r.Host, r.Collection) : null;
    }
}
