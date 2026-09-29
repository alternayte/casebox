using System.Security.Claims;
using Casebox.Server.Features.Orgs;

namespace Casebox.Server.Features.Auth;

public static class CaseboxClaims
{
    public const string Org = "casebox:org";
    public const string Account = "casebox:account";
    public const string Role = "casebox:role";
    public const string Token = "casebox:token";
    public const string TokenKind = "casebox:token_kind";

    public static string OrgId(this ClaimsPrincipal user) =>
        user.FindFirstValue(Org) ?? throw new InvalidOperationException("The request has no organisation.");

    public static string? AccountId(this ClaimsPrincipal user) => user.FindFirstValue(Account);

    public static Role? AccountRole(this ClaimsPrincipal user) =>
        Enum.TryParse<Role>(user.FindFirstValue(Role), ignoreCase: true, out var role) ? role : null;

    // Who acted, for event metadata: an account or a token, never a captured person.
    public static string? Actor(this ClaimsPrincipal user) =>
        user.AccountId() is { } account ? $"account:{account}"
        : user.FindFirstValue(Token) is { } token ? $"token:{token}"
        : null;
}

public static class Policies
{
    public const string Viewer = "viewer";
    public const string Member = "member";
    public const string Admin = "admin";
    public const string Owner = "owner";
    public const string Worker = "worker";
    public const string Ingest = "ingest";
    public const string WorkerOrIngest = "worker_or_ingest";
}
