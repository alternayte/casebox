using System.Security.Claims;
using System.Text.Encodings.Web;
using Casebox.Server.Features.Auth;
using Microsoft.AspNetCore.Authentication;
using Microsoft.Extensions.Options;

namespace Casebox.Server.Features.Tokens;

// Authenticates "Authorization: Bearer cbx_…" worker and ingest tokens.
public sealed class TokenAuthenticationHandler(
    IOptionsMonitor<AuthenticationSchemeOptions> options,
    ILoggerFactory logger,
    UrlEncoder encoder,
    TokenStore tokens) : AuthenticationHandler<AuthenticationSchemeOptions>(options, logger, encoder)
{
    public const string SchemeName = "casebox-token";

    protected override async Task<AuthenticateResult> HandleAuthenticateAsync()
    {
        var header = Request.Headers.Authorization.ToString();
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase)) return AuthenticateResult.NoResult();

        var token = await tokens.AuthenticateAsync(header["Bearer ".Length..].Trim(), Context.RequestAborted);
        if (token is null) return AuthenticateResult.Fail("The token is unknown or revoked.");

        var identity = new ClaimsIdentity(
            [
                new Claim(CaseboxClaims.Org, token.OrgId),
                new Claim(CaseboxClaims.Token, token.Id),
                new Claim(CaseboxClaims.TokenKind, TokenStore.KindName(token.Kind)),
            ],
            SchemeName);
        return AuthenticateResult.Success(new AuthenticationTicket(new ClaimsPrincipal(identity), SchemeName));
    }
}
