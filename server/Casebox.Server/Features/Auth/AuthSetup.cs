using System.Security.Claims;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Tokens;
using Microsoft.AspNetCore.Antiforgery;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Authentication.Cookies;
using Microsoft.AspNetCore.Authentication.OpenIdConnect;
using Microsoft.IdentityModel.Protocols.OpenIdConnect;

namespace Casebox.Server.Features.Auth;

public static class AuthSetup
{
    public const string CookieScheme = CookieAuthenticationDefaults.AuthenticationScheme;
    public const string OidcScheme = OpenIdConnectDefaults.AuthenticationScheme;
    public const string CsrfHeader = "X-CSRF-TOKEN";
    private const string DefaultScheme = "casebox";

    public static IServiceCollection AddCaseboxAuth(
        this IServiceCollection services,
        CaseboxOptions options
    )
    {
        services.AddSingleton<AccountStore>();
        services.AddSingleton<TokenStore>();
        services.AddAntiforgery(o =>
        {
            o.HeaderName = CsrfHeader;
            o.Cookie.Name = "casebox.csrf";
            o.Cookie.SameSite = SameSiteMode.Strict;
            o.Cookie.SecurePolicy = CookieSecurePolicy.SameAsRequest;
        });

        var auth = services
            .AddAuthentication(DefaultScheme)
            // A bearer header means a worker or ingest token; anything else is the UI's cookie.
            .AddPolicyScheme(
                DefaultScheme,
                DefaultScheme,
                o =>
                    o.ForwardDefaultSelector = http =>
                        http
                            .Request.Headers.Authorization.ToString()
                            .StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)
                            ? TokenAuthenticationHandler.SchemeName
                            : CookieScheme
            )
            .AddScheme<AuthenticationSchemeOptions, TokenAuthenticationHandler>(
                TokenAuthenticationHandler.SchemeName,
                null
            )
            .AddCookie(
                CookieScheme,
                o =>
                {
                    o.Cookie.Name = "casebox.session";
                    o.Cookie.HttpOnly = true;
                    o.Cookie.SameSite = SameSiteMode.Strict;
                    o.Cookie.SecurePolicy = CookieSecurePolicy.SameAsRequest;
                    o.ExpireTimeSpan = TimeSpan.FromHours(12);
                    o.SlidingExpiration = true;
                    // An API answers 401 and 403; it never redirects to a login page.
                    o.Events.OnRedirectToLogin = ctx =>
                    {
                        ctx.Response.StatusCode = StatusCodes.Status401Unauthorized;
                        return Task.CompletedTask;
                    };
                    o.Events.OnRedirectToAccessDenied = ctx =>
                    {
                        ctx.Response.StatusCode = StatusCodes.Status403Forbidden;
                        return Task.CompletedTask;
                    };
                    // Role changes and removed accounts apply to the next request, not the next login.
                    o.Events.OnValidatePrincipal = async ctx =>
                    {
                        var user = ctx.Principal!;
                        var accounts =
                            ctx.HttpContext.RequestServices.GetRequiredService<AccountStore>();
                        var role = user.AccountId() is { } id
                            ? await accounts.RoleOfAsync(
                                user.OrgId(),
                                id,
                                ctx.HttpContext.RequestAborted
                            )
                            : null;
                        if (role is null)
                        {
                            ctx.RejectPrincipal();
                            return;
                        }

                        if (role != user.AccountRole())
                        {
                            var identity = (ClaimsIdentity)user.Identity!;
                            identity.RemoveClaim(identity.FindFirst(CaseboxClaims.Role));
                            identity.AddClaim(
                                new Claim(CaseboxClaims.Role, AccountStore.RoleName(role.Value))
                            );
                            ctx.ShouldRenew = true;
                        }
                    };
                }
            );

        if (options.Oidc.Enabled)
        {
            auth.AddOpenIdConnect(
                OidcScheme,
                o =>
                {
                    o.Authority = options.Oidc.Authority;
                    o.ClientId = options.Oidc.ClientId;
                    o.ClientSecret = options.Oidc.ClientSecret;
                    o.ResponseType = OpenIdConnectResponseType.Code;
                    o.ResponseMode = OpenIdConnectResponseMode.Query;
                    o.UsePkce = true;
                    o.SaveTokens = false;
                    o.SignInScheme = CookieScheme;
                    o.CallbackPath = "/api/v1/auth/oidc/callback";
                    o.RequireHttpsMetadata = options.Oidc.Authority!.StartsWith(
                        "https://",
                        StringComparison.OrdinalIgnoreCase
                    );
                    o.Scope.Clear();
                    o.Scope.Add("openid");
                    o.Scope.Add("profile");
                    o.MapInboundClaims = false;
                    // Like the session cookie: Secure over HTTPS, and usable on the http://localhost of a local trial.
                    o.CorrelationCookie.SameSite = SameSiteMode.Lax;
                    o.CorrelationCookie.SecurePolicy = CookieSecurePolicy.SameAsRequest;
                    o.NonceCookie.SameSite = SameSiteMode.Lax;
                    o.NonceCookie.SecurePolicy = CookieSecurePolicy.SameAsRequest;
                    o.Events.OnTokenValidated = async ctx =>
                    {
                        var principal = ctx.Principal!;
                        var issuer = principal.FindFirstValue("iss") ?? options.Oidc.Authority!;
                        var subject =
                            principal.FindFirstValue("sub")
                            ?? throw new InvalidOperationException(
                                "The identity provider returned no subject."
                            );
                        var name =
                            principal.FindFirstValue("name")
                            ?? principal.FindFirstValue("preferred_username")
                            ?? subject;
                        var firstRole = options.Oidc.Owners.Contains(subject)
                            ? Role.Owner
                            : Role.Viewer;
                        var accounts =
                            ctx.HttpContext.RequestServices.GetRequiredService<AccountStore>();
                        var account = await accounts.LoginAsync(
                            options.Org.Id,
                            issuer,
                            subject,
                            name,
                            firstRole,
                            ctx.HttpContext.RequestAborted
                        );
                        ctx.Principal = SessionPrincipal(options.Org.Id, account);
                    };
                }
            );
        }

        services
            .AddAuthorizationBuilder()
            .AddPolicy(
                Policies.Viewer,
                p => p.RequireAssertion(c => c.User.AccountRole() >= Role.Viewer)
            )
            .AddPolicy(
                Policies.Member,
                p => p.RequireAssertion(c => c.User.AccountRole() >= Role.Member)
            )
            .AddPolicy(
                Policies.Admin,
                p => p.RequireAssertion(c => c.User.AccountRole() >= Role.Admin)
            )
            .AddPolicy(
                Policies.Owner,
                p => p.RequireAssertion(c => c.User.AccountRole() >= Role.Owner)
            )
            .AddPolicy(Policies.Worker, p => p.RequireClaim(CaseboxClaims.TokenKind, "worker"))
            .AddPolicy(Policies.Ingest, p => p.RequireClaim(CaseboxClaims.TokenKind, "ingest"))
            .AddPolicy(
                Policies.WorkerOrIngest,
                p => p.RequireClaim(CaseboxClaims.TokenKind, "worker", "ingest")
            )
            .AddPolicy(
                Policies.WorkerOrCi,
                p => p.RequireClaim(CaseboxClaims.TokenKind, "worker", "ci")
            )
            .AddPolicy(
                Policies.CiOrMember,
                p =>
                    p.RequireAssertion(c =>
                        c.User.IsCiToken() || c.User.AccountRole() >= Role.Member
                    )
            )
            .AddPolicy(
                Policies.CiOrViewer,
                p =>
                    p.RequireAssertion(c =>
                        c.User.IsCiToken() || c.User.AccountRole() >= Role.Viewer
                    )
            );

        return services;
    }

    public static ClaimsPrincipal SessionPrincipal(string orgId, Account account) =>
        new(
            new ClaimsIdentity(
                [
                    new Claim(CaseboxClaims.Org, orgId),
                    // A stable user ID, so the CSRF token survives a role change.
                    new Claim(ClaimTypes.NameIdentifier, account.Id),
                    new Claim(CaseboxClaims.Account, account.Id),
                    new Claim(CaseboxClaims.Role, AccountStore.RoleName(account.Role)),
                ],
                CookieScheme
            )
        );

    // Cookie-authenticated requests that change state must carry the CSRF header.
    public static RouteGroupBuilder RequireCsrf(this RouteGroupBuilder group) =>
        group.AddEndpointFilter(
            async (ctx, next) =>
            {
                var http = ctx.HttpContext;
                var unsafeMethod =
                    !HttpMethods.IsGet(http.Request.Method)
                    && !HttpMethods.IsHead(http.Request.Method)
                    && !HttpMethods.IsOptions(http.Request.Method);
                if (
                    unsafeMethod
                    && http.User.Identity
                        is { IsAuthenticated: true, AuthenticationType: CookieScheme }
                )
                {
                    var antiforgery = http.RequestServices.GetRequiredService<IAntiforgery>();
                    if (!await antiforgery.IsRequestValidAsync(http))
                        return Results.Problem(
                            statusCode: StatusCodes.Status403Forbidden,
                            title: "The CSRF token is missing or invalid."
                        );
                }

                return await next(ctx);
            }
        );
}
