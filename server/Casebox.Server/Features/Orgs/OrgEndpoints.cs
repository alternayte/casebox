using Casebox.Server.Features.Auth;
using Deedbox;

namespace Casebox.Server.Features.Orgs;

public static class OrgEndpoints
{
    public sealed record OrgView(string Id, string Name, OrgSettings Settings);

    public static void MapOrg(this RouteGroupBuilder api)
    {
        var org = api.MapGroup("/org").WithTags("Organisation");

        org.MapGet("/", async (HttpContext http, IEventStore store) =>
        {
            var (state, _) = await store.Load<Organisation>(Organisation.StreamId);
            return state.Exists ? Results.Ok(new OrgView(http.User.OrgId(), state.Name, state.Settings)) : Results.NotFound();
        }).RequireAuthorization(Policies.Viewer);

        // Privacy settings (prompt mode, k, pseudonym period) and budgets. Every change is an
        // audited org.settings_changed event carrying the acting account.
        org.MapPut("/settings", async (OrgSettings settings, IEventStore store) =>
        {
            var result = await store.Execute<Organisation>(Organisation.StreamId, state => OrgDecider.ChangeSettings(state, settings));
            return Results.Ok(result.State.Settings);
        }).RequireAuthorization(Policies.Admin);
    }
}
