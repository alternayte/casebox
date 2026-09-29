namespace Casebox.Server;

// A rule of the domain refused the request. The API returns 422 with the message and its code
// (docs/specs/operations.md, Error codes); CBX001 when the refusal has no code of its own.
public class DomainException(string message, string? code = null) : Exception(message)
{
    public virtual string Code => code ?? Cbx.Refused;
}

// The thing the request names does not exist in the caller's organisation. The API returns 404.
public sealed class NotFoundException(string message, string? code = null)
    : DomainException(message, code ?? Cbx.NotFound);

// The request conflicts with the current state. The API returns 409.
public sealed class ConflictException(string message, string? code = null)
    : DomainException(message, code ?? Cbx.Conflict);

// Casebox's error codes. Each has a page in docs/src/content/docs/reference/errors/;
// checks/error-codes.sh keeps the codes and the pages in step.
public static class Cbx
{
    public const string Docs = "https://casebox-docs.pages.dev/reference/errors/";

    public const string Refused = "CBX001";
    public const string NotFound = "CBX002";
    public const string Conflict = "CBX003";
    public const string Unauthenticated = "CBX010";
    public const string Forbidden = "CBX011";
    public const string NoWorkspace = "CBX040";
    public const string GitHubRefused = "CBX071";
    public const string JiraRefused = "CBX072";
    public const string BelowK = "CBX080";
    public const string NoPromptMode = "CBX090";

    public static string Url(string code) => $"{Docs}{code.ToLowerInvariant()}/";

    // A problem answer with its code, for routes that answer without an exception.
    public static IResult Problem(int status, string code, string title) =>
        Results.Problem(
            statusCode: status,
            title: title,
            type: Url(code),
            extensions: new Dictionary<string, object?> { ["code"] = code }
        );
}
