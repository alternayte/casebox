using Casebox.Server.Features.Auth;
using Deedbox;

namespace Casebox.Server.Infrastructure;

// One organisation is one Deedbox tenant. Every store in the request scope reads and writes in
// the caller's organisation, and every event records who acted.
public sealed class TenancyMiddleware(RequestDelegate next)
{
    public Task InvokeAsync(HttpContext http, DeedboxContext deedbox)
    {
        if (http.User.Identity?.IsAuthenticated == true)
        {
            deedbox.TenantId = http.User.OrgId();
            deedbox.Metadata = new EventMetadata
            {
                CorrelationId = http.TraceIdentifier,
                Actor = http.User.Actor(),
            };
        }

        return next(http);
    }
}
