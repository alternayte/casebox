using Casebox.Server.Features.Auth;
using Deedbox;

namespace Casebox.Server.Features.Tokens;

public static class TokenEndpoints
{
    public sealed record IssueToken(TokenKind Kind, string Name);

    public static void MapTokens(this RouteGroupBuilder api)
    {
        var tokens = api.MapGroup("/tokens")
            .WithTags("Tokens")
            .RequireAuthorization(Policies.Admin);

        tokens.MapGet(
            "/",
            async (HttpContext http, TokenStore store) =>
                Results.Ok(await store.ListAsync(http.User.OrgId(), http.RequestAborted))
        );

        // The response is the only time the secret is shown.
        tokens.MapPost(
            "/",
            async (IssueToken body, HttpContext http, TokenStore store, IEventStore events) =>
            {
                if (body.Kind == TokenKind.Cli)
                    return Results.Problem(
                        statusCode: StatusCodes.Status422UnprocessableEntity,
                        title: "A CLI token comes only from a device login."
                    );
                var issued = await store.IssueAsync(
                    http.User.OrgId(),
                    body.Kind,
                    body.Name,
                    http.User.Actor()!,
                    events,
                    http.RequestAborted
                );
                return Results.Created($"/api/v1/tokens/{issued.Info.Id}", issued);
            }
        );

        // A member's machine gets its own ingest token, named without the host so no identity is stored.
        api.MapPost(
                "/devices",
                async (HttpContext http, TokenStore store, IEventStore events) =>
                {
                    var name =
                        $"device {Convert.ToHexStringLower(System.Security.Cryptography.RandomNumberGenerator.GetBytes(4))}";
                    var issued = await store.IssueAsync(
                        http.User.OrgId(),
                        TokenKind.Ingest,
                        name,
                        http.User.Actor()!,
                        events,
                        http.RequestAborted
                    );
                    return Results.Created($"/api/v1/tokens/{issued.Info.Id}", issued);
                }
            )
            .WithTags("Tokens")
            .RequireAuthorization(Policies.Member);

        tokens.MapDelete(
            "/{id}",
            async (string id, HttpContext http, TokenStore store, IEventStore events) =>
            {
                await store.RevokeAsync(http.User.OrgId(), id, events, http.RequestAborted);
                return Results.NoContent();
            }
        );
    }
}
