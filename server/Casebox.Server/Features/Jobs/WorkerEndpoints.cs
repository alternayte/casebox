using System.Text.Json;
using Casebox.Server.Features.Auth;
using Deedbox;

namespace Casebox.Server.Features.Jobs;

// Workers call the server; the server never calls a worker.
public static class WorkerEndpoints
{
    public sealed record LeaseRequest(string WorkerId, string Version, IReadOnlyList<string> Kinds);

    public sealed record WorkerRef(string WorkerId);

    public sealed record CompleteRequest(string WorkerId, JsonElement Result);

    public sealed record FailRequest(string WorkerId, string Error, bool Retryable);

    public static void MapWorkerJobs(this RouteGroupBuilder worker)
    {
        var jobs = worker
            .MapGroup("/jobs")
            .WithTags("Worker")
            .RequireAuthorization(Policies.Worker);

        jobs.MapPost(
            "/lease",
            async (LeaseRequest body, HttpContext http, JobQueue queue) =>
            {
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
                    http.RequestAborted
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
