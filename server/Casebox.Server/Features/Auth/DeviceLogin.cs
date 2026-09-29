using System.Security.Cryptography;
using System.Text;
using Casebox.Server.Features.Tokens;
using Dapper;
using Deedbox;
using Microsoft.Extensions.Options;
using Npgsql;

namespace Casebox.Server.Features.Auth;

// Device login for the CLI (RFC 8628): the CLI shows a code, a logged-in person approves it in
// the UI, and the CLI's poll receives a CLI token for that person's account, once.
public static class DeviceLogin
{
    public static readonly TimeSpan Lifetime = TimeSpan.FromMinutes(10);
    public const int IntervalSeconds = 5;

    // No vowels or look-alike letters, so a code never spells a word and reads back clearly.
    private const string Alphabet = "BCDFGHJKLMNPQRSTVWXZ";

    public sealed record CodeResponse(
        string DeviceCode,
        string UserCode,
        string VerificationUri,
        int ExpiresIn,
        int Interval
    );

    public sealed record Approve(string UserCode);

    public sealed record TokenRequest(string DeviceCode);

    public sealed record TokenResponse(string AccessToken, string OrgId);

    public static void MapDeviceLogin(this RouteGroupBuilder api)
    {
        var device = api.MapGroup("/auth/device").WithTags("Auth");

        device
            .MapPost(
                "/code",
                async (
                    HttpContext http,
                    NpgsqlDataSource db,
                    IOptions<CaseboxOptions> options,
                    TimeProvider clock
                ) =>
                {
                    var deviceCode = Convert.ToHexStringLower(RandomNumberGenerator.GetBytes(32));
                    var userCode = NewUserCode();
                    var now = clock.GetUtcNow();
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    await connection.ExecuteAsync(
                        new CommandDefinition(
                            """
                            DELETE FROM casebox.device_codes WHERE expires_at < @Now;
                            INSERT INTO casebox.device_codes (device_code_hash, user_code, org_id, status, expires_at, created_at)
                            VALUES (@Hash, @UserCode, @Org, 'pending', @Expires, @Now)
                            """,
                            new
                            {
                                Hash = Hash(deviceCode),
                                UserCode = userCode,
                                Org = options.Value.Org.Id,
                                Expires = now + Lifetime,
                                Now = now,
                            },
                            cancellationToken: http.RequestAborted
                        )
                    );

                    var uri = $"{http.Request.Scheme}://{http.Request.Host}/device";
                    return Results.Ok(
                        new CodeResponse(
                            deviceCode,
                            userCode,
                            uri,
                            (int)Lifetime.TotalSeconds,
                            IntervalSeconds
                        )
                    );
                }
            )
            .AllowAnonymous()
            .RequireRateLimiting(AuthEndpoints.LoginRateLimit);

        device
            .MapPost(
                "/approve",
                async (Approve body, HttpContext http, NpgsqlDataSource db, TimeProvider clock) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    var approved = await connection.ExecuteAsync(
                        new CommandDefinition(
                            """
                            UPDATE casebox.device_codes SET status = 'approved', account_id = @Account
                            WHERE user_code = @UserCode AND org_id = @Org AND status = 'pending' AND expires_at > @Now
                            """,
                            new
                            {
                                UserCode = Normalize(body.UserCode),
                                Account = http.User.AccountId(),
                                Org = http.User.OrgId(),
                                Now = clock.GetUtcNow(),
                            },
                            cancellationToken: http.RequestAborted
                        )
                    );
                    return approved == 1
                        ? Results.NoContent()
                        : Results.Problem(
                            statusCode: StatusCodes.Status404NotFound,
                            title: "The code is unknown, used or expired."
                        );
                }
            )
            .RequireAuthorization(Policies.Viewer);

        device
            .MapPost(
                "/token",
                async (
                    TokenRequest body,
                    HttpContext http,
                    NpgsqlDataSource db,
                    TokenStore tokens,
                    IServiceProvider services,
                    TimeProvider clock
                ) =>
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    await using var transaction = await connection.BeginTransactionAsync(
                        http.RequestAborted
                    );
                    var row = await connection.QuerySingleOrDefaultAsync<(
                        string OrgId,
                        string Status,
                        string? AccountId,
                        DateTime ExpiresAt
                    )?>(
                        new CommandDefinition(
                            "SELECT org_id, status, account_id, expires_at FROM casebox.device_codes WHERE device_code_hash = @Hash FOR UPDATE",
                            new { Hash = Hash(body.DeviceCode ?? "") },
                            transaction,
                            cancellationToken: http.RequestAborted
                        )
                    );

                    if (row is not { } code || code.Status == "consumed")
                        return Results.Problem(
                            statusCode: StatusCodes.Status400BadRequest,
                            title: "invalid_grant"
                        );
                    if (
                        new DateTimeOffset(DateTime.SpecifyKind(code.ExpiresAt, DateTimeKind.Utc))
                        < clock.GetUtcNow()
                    )
                        return Results.Problem(
                            statusCode: StatusCodes.Status410Gone,
                            title: "expired_token"
                        );
                    if (code.Status == "pending")
                        return Results.Problem(
                            statusCode: StatusCodes.Status428PreconditionRequired,
                            title: "authorization_pending"
                        );

                    await connection.ExecuteAsync(
                        new CommandDefinition(
                            "UPDATE casebox.device_codes SET status = 'consumed' WHERE device_code_hash = @Hash",
                            new { Hash = Hash(body.DeviceCode!) },
                            transaction,
                            cancellationToken: http.RequestAborted
                        )
                    );

                    // The request is anonymous, so the event store's tenant and actor are set here.
                    var context = services.GetRequiredService<DeedboxContext>();
                    context.TenantId = code.OrgId;
                    context.Metadata = new EventMetadata
                    {
                        CorrelationId = http.TraceIdentifier,
                        Actor = $"account:{code.AccountId}",
                    };
                    var store = services.GetRequiredService<IEventStore>();
                    var issued = await tokens.IssueAsync(
                        transaction,
                        code.OrgId,
                        TokenKind.Cli,
                        "cli",
                        $"account:{code.AccountId}",
                        code.AccountId,
                        store,
                        http.RequestAborted
                    );
                    await transaction.CommitAsync(http.RequestAborted);
                    return Results.Ok(new TokenResponse(issued.Secret, code.OrgId));
                }
            )
            .AllowAnonymous();
    }

    private static string NewUserCode()
    {
        var chars = new char[8];
        for (var i = 0; i < chars.Length; i++)
            chars[i] = Alphabet[RandomNumberGenerator.GetInt32(Alphabet.Length)];
        return $"{new string(chars, 0, 4)}-{new string(chars, 4, 4)}";
    }

    private static string Normalize(string? code)
    {
        var letters = new string((code ?? "").ToUpperInvariant().Where(char.IsLetter).ToArray());
        return letters.Length == 8 ? $"{letters[..4]}-{letters[4..]}" : letters;
    }

    private static byte[] Hash(string value) => SHA256.HashData(Encoding.UTF8.GetBytes(value));
}
