namespace Casebox.Server;

// A rule of the domain refused the request. The API returns 422 with the message.
public class DomainException(string message) : Exception(message);

// The thing the request names does not exist in the caller's organisation. The API returns 404.
public sealed class NotFoundException(string message) : DomainException(message);

// The request conflicts with the current state. The API returns 409.
public sealed class ConflictException(string message) : DomainException(message);
