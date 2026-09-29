using System.Text.Json;
using System.Text.Json.Serialization;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Proposals;
using Casebox.Server.Features.Steering;
using Casebox.Server.Features.WorkItems;
using Casebox.Server.Features.Workspaces;
using Deedbox;

namespace Casebox.Server.Infrastructure;

// Every stream and projection, and the JSON shape of events. The event contract test verifies
// this same registration.
public static class CaseboxStreams
{
    // Enums are stored and served as snake_case names, never as numbers.
    public static readonly JsonStringEnumConverter Enums = new(JsonNamingPolicy.SnakeCaseLower);

    public static DeedboxBuilder Register(DeedboxBuilder es) =>
        es.ConfigureJson(o => o.Converters.Add(Enums))
            .Stream<Organisation>("org", s => s.EventsNestedIn(typeof(OrgEvents)))
            .Stream<Workspace>(s => s.EventsNestedIn(typeof(WorkspaceEvents)))
            .Stream<WorkItem>("work_item", s => s.EventsNestedIn(typeof(WorkItemEvents)))
            .Stream<SteeringState>("steering", s => s.EventsNestedIn(typeof(SteeringEvents)))
            .Stream<Pattern>("pattern", s => s.EventsNestedIn(typeof(PatternEvents)))
            .Stream<Proposal>("proposal", s => s.EventsNestedIn(typeof(ProposalEvents)))
            .Projection<WorkspaceProjection>(WorkspaceProjection.Name, Run.Inline)
            .Projection<WorkItemProjection>(WorkItemProjection.Name, Run.Inline)
            .Projection<SteeringFacts>(SteeringFacts.Name, Run.Inline)
            .Projection<PatternBoard>(PatternBoard.Name, Run.Inline)
            .Projection<ProposalBoard>(ProposalBoard.Name, Run.Inline);
}
