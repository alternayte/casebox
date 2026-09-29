using System.Text.Json;
using Casebox.Server.Features.Demo;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Patterns;
using Casebox.Server.Features.Proposals;
using Casebox.Server.Tests.Infrastructure;
using Dapper;
using Deedbox;
using Deedbox.Testing;
using Microsoft.Extensions.DependencyInjection;
using Npgsql;

namespace Casebox.Server.Tests.Features;

public sealed class ProposalDeciderTests
{
    private static readonly ProposalEvents.Drafted Edit = new(
        "p1",
        "demo",
        "github.com/acme/payments",
        ProposalKind.HarnessEdit,
        "Say that integration tests use the real database",
        "why",
        [new Edit("add_bullet", "AGENTS.md", "Testing", null, "Use the real database.")],
        [],
        null,
        "h1",
        "abc",
        null
    );

    private static readonly ProposalEvents.Drafted Note = Edit with
    {
        Kind = ProposalKind.CodeNote,
        Edits = [],
        Note = new CodeNote("what", "why", "prompt"),
    };

    private static readonly DateTimeOffset At = DateTimeOffset.UnixEpoch;

    [Fact]
    public void Only_an_approved_proposal_is_applied() =>
        Decider
            .Given<Proposal>(Edit)
            .When(p => ProposalDecider.Apply(p, ApplyMode.Private, "me", At))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_private_apply_can_be_committed_later_but_never_made_private_again()
    {
        Decider
            .Given<Proposal>(
                Edit,
                new ProposalEvents.Approved("me"),
                new ProposalEvents.Applied(ApplyMode.Private, "me", At)
            )
            .When(p => ProposalDecider.Apply(p, ApplyMode.Commit, "me", At))
            .Then(new ProposalEvents.Applied(ApplyMode.Commit, "me", At));
        Decider
            .Given<Proposal>(
                Edit,
                new ProposalEvents.Approved("me"),
                new ProposalEvents.Applied(ApplyMode.Commit, "me", At)
            )
            .When(p => ProposalDecider.Apply(p, ApplyMode.Private, "me", At))
            .ThenNothing();
    }

    [Fact]
    public void A_code_note_cannot_stay_private() =>
        Decider
            .Given<Proposal>(Note, new ProposalEvents.Approved("me"))
            .When(p => ProposalDecider.Apply(p, ApplyMode.Private, "me", At))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_rejection_needs_a_reason_and_an_applied_change_is_reverted_instead()
    {
        Decider
            .Given<Proposal>(Edit)
            .When(p => ProposalDecider.Reject(p, " ", "me"))
            .ThenThrows<DomainException>();
        Decider
            .Given<Proposal>(
                Edit,
                new ProposalEvents.Approved("me"),
                new ProposalEvents.Applied(ApplyMode.Private, "me", At)
            )
            .When(p => ProposalDecider.Reject(p, "not useful", "me"))
            .ThenThrows<DomainException>();
    }
}

// The draft loop on the demo organisation: at most three proposals wait for a person, a rejection
// frees a place, and a draft that repeats a rejected change is dropped.
public sealed class ProposalTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task Drafts_fill_free_places_and_never_bring_back_a_rejected_change()
    {
        var org = $"prop-{Guid.NewGuid():N}"[..20];
        using var scope = stack.ServerA.Services.CreateScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = org;
        var store = scope.ServiceProvider.GetRequiredService<IEventStore>();
        await store.Execute<Organisation>(
            Organisation.StreamId,
            o => OrgDecider.Create(o, "Proposals"),
            Ct
        );
        Assert.True(await scope.ServiceProvider.GetRequiredService<DemoSeed>().SeedAsync(Ct));
        var jobs = scope.ServiceProvider.GetRequiredService<JobQueue>();
        await using var db = new NpgsqlConnection(stack.ConnectionString);
        await db.OpenAsync(Ct);
        var workspaces = await PatternScan.WorkspacesAsync(db, org, Ct);

        // Two open proposals and every proposable pattern has one: nothing to draft.
        await ProposalJobs.EnqueueDraftsAsync(db, jobs, org, workspaces, Ct);
        Assert.Null(await jobs.LeaseAsync(org, "w", "t", [ProposalJobs.Draft], Ct));

        var skill = await db.QuerySingleAsync<(string Id, string Pattern)>(
            "SELECT id, pattern FROM casebox.proposals WHERE org_id = @Org AND kind = 'skill'",
            new { Org = org }
        );
        await store.Execute<Proposal>(
            Proposal.StreamId(skill.Id),
            p => ProposalDecider.Reject(p, "we have no ledger service yet", "account:me"),
            Ct
        );

        // The rejection frees the place and its pattern gets a draft job, once.
        await ProposalJobs.EnqueueDraftsAsync(db, jobs, org, workspaces, Ct);
        await ProposalJobs.EnqueueDraftsAsync(db, jobs, org, workspaces, Ct);
        var job = await jobs.LeaseAsync(org, "w", "t", [ProposalJobs.Draft], Ct);
        Assert.NotNull(job);
        Assert.Equal(skill.Pattern, job.Payload.GetProperty("pattern").GetString());
        Assert.Null(await jobs.LeaseAsync(org, "w2", "t", [ProposalJobs.Draft], Ct));

        // The model drafts the rejected change again: no proposal comes of it.
        var rejected = await db.QuerySingleAsync<string>(
            "SELECT edits::text FROM casebox.proposals WHERE org_id = @Org AND id = @Id",
            new { Org = org, skill.Id }
        );
        var answer = JsonSerializer.SerializeToElement(
            new
            {
                kind = "skill",
                title = "Add an outbox skill again",
                rationale = "same",
                edits = JsonDocument.Parse(rejected).RootElement,
                preview = Array.Empty<object>(),
                baseCommit = "abc",
            }
        );
        Assert.Equal(
            JobOutcome.Done,
            await jobs.CompleteAsync(org, job.Id, "w", answer, store, scope.ServiceProvider, Ct)
        );
        Assert.Equal(
            3,
            await db.ExecuteScalarAsync<int>(
                "SELECT count(*) FROM casebox.proposals WHERE org_id = @Org",
                new { Org = org }
            )
        );
    }
}
