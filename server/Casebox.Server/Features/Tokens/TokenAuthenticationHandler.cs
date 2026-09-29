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
    TokenStore tokens,
    AccountStore accounts
) : AuthenticationHandler<AuthenticationSchemeOptions>(options, logger, encoder)
{
    public const string SchemeName = "casebox-token";

    protected override async Task<AuthenticateResult> HandleAuthenticateAsync()
    {
        var header = Request.Headers.Authorization.ToString();
        if (!header.StartsWith("Bearer ", StringComparison.OrdinalIgnoreCase))
            return AuthenticateResult.NoResult();

        var token = await tokens.AuthenticateAsync(
            header["Bearer ".Length..].Trim(),
            Context.RequestAborted
        );
        if (token is null)
            return AuthenticateResult.Fail("The token is unknown or revoked.");

        var claims = new List<Claim>
        {
            new(CaseboxClaims.Org, token.OrgId),
            new(CaseboxClaims.Token, token.Id),
            new(CaseboxClaims.TokenKind, TokenStore.KindName(token.Kind)),
        };

        // A CLI token acts as its account, with the role the account has now.
        if (token.AccountId is { } accountId)
        {
            var role = await accounts.RoleOfAsync(token.OrgId, accountId, Context.RequestAborted);
            if (role is null)
                return AuthenticateResult.Fail("The token's account no longer exists.");
            claims.Add(new Claim(ClaimTypes.NameIdentifier, accountId));
            claims.Add(new Claim(CaseboxClaims.Account, accountId));
            claims.Add(new Claim(CaseboxClaims.Role, AccountStore.RoleName(role.Value)));
        }

        var identity = new ClaimsIdentity(claims, SchemeName);
        return AuthenticateResult.Success(
            new AuthenticationTicket(new ClaimsPrincipal(identity), SchemeName)
        );
    }
}
