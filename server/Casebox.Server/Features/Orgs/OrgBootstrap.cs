using Deedbox;
using Microsoft.Extensions.Options;

namespace Casebox.Server.Features.Orgs;

// Creates the configured organisation on first start. Safe to run on every start.
public sealed class OrgBootstrap(IServiceProvider services, IOptions<CaseboxOptions> options)
{
    public async Task EnsureAsync()
    {
        await using var scope = services.CreateAsyncScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = options.Value.Org.Id;
        context.Metadata = new EventMetadata { Actor = "system:bootstrap" };
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        await store.Execute<Organisation>(
            Organisation.StreamId,
            org => OrgDecider.Create(org, options.Value.Org.Name)
        );
        if (options.Value.Demo)
        {
            context.Metadata = new EventMetadata { Actor = "system:demo" };
            await scope.ServiceProvider.GetRequiredService<Demo.DemoSeed>().SeedAsync(default);
        }
    }
}
