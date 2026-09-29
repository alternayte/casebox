using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Ci;
using Microsoft.AspNetCore.Http.Features;
using Npgsql;

namespace Casebox.Server.Features.Blobs;

public static class BlobEndpoints
{
    public static void MapWorkerBlobs(this RouteGroupBuilder worker)
    {
        var blobs = worker.MapGroup("/blobs").WithTags("Worker");

        // Workers and ingest tokens upload; a repeat upload of the same content is a no-op.
        blobs
            .MapPut(
                "/{hash}",
                async (string hash, HttpContext http, BlobStore store) =>
                {
                    var size = http.Features.Get<IHttpMaxRequestBodySizeFeature>();
                    if (size is { IsReadOnly: false })
                        size.MaxRequestBodySize = BlobStore.MaxSize;
                    using var buffer = new MemoryStream();
                    await http.Request.Body.CopyToAsync(buffer, http.RequestAborted);
                    var contentType = string.IsNullOrEmpty(http.Request.ContentType)
                        ? "application/octet-stream"
                        : http.Request.ContentType;
                    var created = await store.PutAsync(
                        http.User.OrgId(),
                        hash,
                        contentType,
                        buffer.ToArray(),
                        http.RequestAborted
                    );
                    return created
                        ? Results.Created($"/worker/v1/blobs/{hash}", null)
                        : Results.NoContent();
                }
            )
            .RequireAuthorization(p =>
                p.RequireClaim(CaseboxClaims.TokenKind, "worker", "ingest", "ci")
            )
            .Accepts<byte[]>("application/octet-stream");

        blobs
            .MapGet(
                "/{hash}",
                async (string hash, HttpContext http, BlobStore store, NpgsqlDataSource db) =>
                {
                    var org = http.User.OrgId();
                    if (http.User.IsCiToken())
                    {
                        await using var connection = await db.OpenConnectionAsync(
                            http.RequestAborted
                        );
                        if (
                            !await CiRuns.BlobReadableAsync(
                                connection,
                                org,
                                hash,
                                http.User.TokenId()!,
                                async h =>
                                    (await store.GetAsync(org, h, http.RequestAborted))?.Data,
                                http.RequestAborted
                            )
                        )
                            return Results.NotFound();
                    }
                    var blob = await store.GetAsync(org, hash, http.RequestAborted);
                    return blob is null
                        ? Results.NotFound()
                        : Results.Bytes(blob.Data, blob.ContentType);
                }
            )
            .RequireAuthorization(Policies.WorkerOrCi);
    }
}
