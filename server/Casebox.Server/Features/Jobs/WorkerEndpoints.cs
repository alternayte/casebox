using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Ci;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Jobs;

// Workers call the server; the server never calls a worker.
public static class WorkerEndpoints
{
    // Scope limits the lease to one CI run's jobs; a ci token's inline worker must set it.
    public sealed record LeaseRequest(
        string WorkerId,
        string Version,
        IReadOnlyList<string> Kinds,
        string? Scope = null
    );

    public sealed record WorkerRef(string WorkerId);

    public sealed record CompleteRequest(string WorkerId, JsonElement Result);

    public sealed record FailRequest(string WorkerId, string Error, bool Retryable);

    public static void MapWorkerJobs(this RouteGroupBuilder worker)
    {
        var jobs = worker
            .MapGroup("/jobs")
            .WithTags("Worker")
            .RequireAuthorization(Policies.WorkerOrCi)
            .AddEndpointFilter(CiJobsOnly);

        jobs.MapPost(
            "/lease",
            async (LeaseRequest body, HttpContext http, JobQueue queue, NpgsqlDataSource db) =>
            {
                if (http.User.IsCiToken())
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    if (
                        body.Scope is null
                        || !await CiRuns.OwnedByAsync(
                            connection,
                            http.User.OrgId(),
                            body.Scope,
                            http.User.TokenId()!,
                            http.RequestAborted
                        )
                    )
                        return Results.Problem(
                            statusCode: StatusCodes.Status403Forbidden,
                            title: "A ci token leases only the jobs of a CI run it started."
                        );
                }
                if (string.IsNullOrWhiteSpace(body.WorkerId) || body.Kinds is not { Count: > 0 })
                    return Results.Problem(
                        statusCode: StatusCodes.Status400BadRequest,
                        title: "A lease needs a worker ID and at least one job kind."
                    );
                var job = await queue.LeaseAsync(
                    http.User.OrgId(),
                    body.WorkerId,
                    body.Version ?? "",
                    body.Kinds,
                    http.RequestAborted,
                    body.Scope
                );
                return job is null ? Results.NoContent() : Results.Ok(job);
            }
        );

        jobs.MapPost(
            "/{id}/heartbeat",
            async (string id, WorkerRef body, HttpContext http, JobQueue queue) =>
                await queue.HeartbeatAsync(
                    http.User.OrgId(),
                    id,
                    body.WorkerId,
                    http.RequestAborted
                )
                    ? Results.NoContent()
                    : Results.Problem(
                        statusCode: StatusCodes.Status409Conflict,
                        title: "The lease is lost."
                    )
        );

        jobs.MapPost(
            "/{id}/complete",
            async (
                string id,
                CompleteRequest body,
                HttpContext http,
                JobQueue queue,
                IEventStore store
            ) =>
                Outcome(
                    await queue.CompleteAsync(
                        http.User.OrgId(),
                        id,
                        body.WorkerId,
                        body.Result,
                        store,
                        http.RequestServices,
                        http.RequestAborted
                    )
                )
        );

        jobs.MapPost(
            "/{id}/fail",
            async (string id, FailRequest body, HttpContext http, JobQueue queue) =>
                Outcome(
                    await queue.FailAsync(
                        http.User.OrgId(),
                        id,
                        body.WorkerId,
                        body.Error ?? "",
                        body.Retryable,
                        http.RequestAborted
                    )
                )
        );
    }

    // A ci token's heartbeats, results and failures reach only jobs of the CI runs it started.
    private static async ValueTask<object?> CiJobsOnly(
        EndpointFilterInvocationContext ctx,
        EndpointFilterDelegate next
    )
    {
        var http = ctx.HttpContext;
        if (http.User.IsCiToken() && http.Request.RouteValues.TryGetValue("id", out var id))
        {
            var db = http.RequestServices.GetRequiredService<NpgsqlDataSource>();
            await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
            if (
                !await CiRuns.JobOwnedByAsync(
                    connection,
                    http.User.OrgId(),
                    id?.ToString() ?? "",
                    http.User.TokenId()!,
                    http.RequestAborted
                )
            )
                return Results.NotFound();
        }
        return await next(ctx);
    }

    private static IResult Outcome(JobOutcome outcome) =>
        outcome switch
        {
            JobOutcome.Done => Results.NoContent(),
            JobOutcome.NotFound => Results.NotFound(),
            _ => Results.Problem(
                statusCode: StatusCodes.Status409Conflict,
                title: "The lease is lost."
            ),
        };
}
