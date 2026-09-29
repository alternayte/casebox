using System.Security.Cryptography;
using System.Text;
using Casebox.Server.Features.Orgs;
using Deedbox;
using Microsoft.AspNetCore.Antiforgery;
using Microsoft.AspNetCore.Authentication;
using Microsoft.Extensions.Options;

namespace Casebox.Server.Features.Auth;

public static class AuthEndpoints
{
    public const string LoginRateLimit = "login";

    public sealed record LocalLogin(string Password);

    public sealed record Me(string AccountId, string DisplayName, Role Role, string OrgId);

    public sealed record Csrf(string Token);

    public sealed record RoleChange(Role Role);

    public static void MapAuth(this RouteGroupBuilder api)
    {
        var auth = api.MapGroup("/auth").WithTags("Auth");

        auth.MapPost("/local", async (LocalLogin body, IOptions<CaseboxOptions> options, AccountStore accounts, HttpContext http) =>
        {
            var configured = options.Value.LocalAdmin.Password;
            if (string.IsNullOrEmpty(configured)) return Results.NotFound();
            if (!CryptographicOperations.FixedTimeEquals(Encoding.UTF8.GetBytes(body.Password ?? ""), Encoding.UTF8.GetBytes(configured)))
                return Results.Problem(statusCode: StatusCodes.Status401Unauthorized, title: "The password is wrong.");

            var orgId = options.Value.Org.Id;
            var account = await accounts.LoginAsync(orgId, "local", "admin", "Local admin", Role.Owner, http.RequestAborted);
            await http.SignInAsync(AuthSetup.CookieScheme, AuthSetup.SessionPrincipal(orgId, account));
            return Results.NoContent();
        }).AllowAnonymous().RequireRateLimiting(LoginRateLimit);

        auth.MapGet("/oidc/login", (string? returnUrl, IOptions<CaseboxOptions> options) =>
        {
            if (!options.Value.Oidc.Enabled) return Results.NotFound();
            var target = returnUrl is { Length: > 0 } && returnUrl.StartsWith('/') && !returnUrl.StartsWith("//", StringComparison.Ordinal) ? returnUrl : "/";
            return Results.Challenge(new AuthenticationProperties { RedirectUri = target }, [AuthSetup.OidcScheme]);
        }).AllowAnonymous();

        auth.MapPost("/logout", async (HttpContext http) =>
        {
            await http.SignOutAsync(AuthSetup.CookieScheme);
            return Results.NoContent();
        }).RequireAuthorization(Policies.Viewer);

        auth.MapGet("/csrf", (IAntiforgery antiforgery, HttpContext http) =>
            Results.Ok(new Csrf(antiforgery.GetAndStoreTokens(http).RequestToken!)))
            .RequireAuthorization(Policies.Viewer);

        api.MapGet("/me", async (HttpContext http, AccountStore accounts) =>
        {
            var user = http.User;
            var account = await accounts.GetAsync(user.OrgId(), user.AccountId()!, http.RequestAborted);
            return account is null ? Results.Unauthorized() : Results.Ok(new Me(account.Id, account.DisplayName, account.Role, user.OrgId()));
        }).RequireAuthorization(Policies.Viewer).WithTags("Auth");

        var accountsGroup = api.MapGroup("/accounts").WithTags("Accounts").RequireAuthorization(Policies.Admin);

        accountsGroup.MapGet("/", async (HttpContext http, AccountStore accounts) =>
            Results.Ok(await accounts.ListAsync(http.User.OrgId(), http.RequestAborted)));

        accountsGroup.MapPut("/{id}/role", async (string id, RoleChange body, HttpContext http, AccountStore accounts, IEventStore store) =>
        {
            await accounts.ChangeRoleAsync(http.User.OrgId(), id, body.Role, http.User.AccountRole()!.Value, store, http.RequestAborted);
            return Results.NoContent();
        });
    }
}
