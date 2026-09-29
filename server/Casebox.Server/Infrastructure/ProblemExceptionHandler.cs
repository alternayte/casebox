using Deedbox;
using Microsoft.AspNetCore.Diagnostics;

namespace Casebox.Server.Infrastructure;

// Domain refusals become problem responses; anything else stays a 500.
public sealed class ProblemExceptionHandler(IProblemDetailsService problems) : IExceptionHandler
{
    public async ValueTask<bool> TryHandleAsync(HttpContext http, Exception exception, CancellationToken ct)
    {
        var status = exception switch
        {
            NotFoundException => StatusCodes.Status404NotFound,
            ConflictException or ConcurrencyException => StatusCodes.Status409Conflict,
            DomainException => StatusCodes.Status422UnprocessableEntity,
            BadHttpRequestException bad => bad.StatusCode,
            _ => 0,
        };
        if (status == 0) return false;

        http.Response.StatusCode = status;
        return await problems.TryWriteAsync(new ProblemDetailsContext
        {
            HttpContext = http,
            Exception = exception,
            ProblemDetails = { Status = status, Title = exception.Message },
        });
    }
}
