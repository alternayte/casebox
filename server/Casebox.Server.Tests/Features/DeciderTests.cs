using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.Workspaces;
using Casebox.Server.Infrastructure;
using Deedbox.Testing;

namespace Casebox.Server.Tests.Features;

public sealed class OrgDeciderTests
{
    private static readonly OrgEvents.Created Created = new("Acme", OrgSettings.Defaults);

    [Fact]
    public void A_new_org_has_no_prompt_mode_until_an_admin_chooses_one() =>
        Decider
            .Given<Organisation>()
            .When(org => OrgDecider.Create(org, "Acme"))
            .Then(new OrgEvents.Created("Acme", OrgSettings.Defaults with { PromptMode = null }));

    [Fact]
    public void Creating_an_existing_org_changes_nothing() =>
        Decider
            .Given<Organisation>(Created)
            .When(org => OrgDecider.Create(org, "Other"))
            .ThenNothing();

    [Fact]
    public void K_never_drops_below_two() =>
        Decider
            .Given<Organisation>(Created)
            .When(org => OrgDecider.ChangeSettings(org, org.Settings with { K = 1 }))
            .ThenThrows<DomainException>();

    [Fact]
    public void K_of_two_is_allowed() =>
        Decider
            .Given<Organisation>(Created)
            .When(org => OrgDecider.ChangeSettings(org, org.Settings with { K = 2 }))
            .Then(new OrgEvents.SettingsChanged(OrgSettings.Defaults with { K = 2 }));

    [Fact]
    public void A_chosen_prompt_mode_cannot_be_unset() =>
        Decider
            .Given<Organisation>(
                Created,
                new OrgEvents.SettingsChanged(
                    OrgSettings.Defaults with
                    {
                        PromptMode = PromptMode.Redacted,
                    }
                )
            )
            .When(org => OrgDecider.ChangeSettings(org, org.Settings with { PromptMode = null }))
            .ThenThrows<DomainException>();

    [Fact]
    public void Unchanged_settings_append_nothing() =>
        Decider
            .Given<Organisation>(Created)
            .When(org => OrgDecider.ChangeSettings(org, org.Settings))
            .ThenNothing();
}

public sealed class WorkspaceDeciderTests
{
    private static readonly WorkspaceEvents.Created Created = new("payments");
    private static readonly WorkspaceEvents.RecipeProposed Proposed = new("{}", "h1");

    [Fact]
    public void A_workspace_name_is_a_slug() =>
        Decider
            .Given<Workspace>()
            .When(w => WorkspaceDecider.Create(w, "Payments API"))
            .ThenThrows<DomainException>();

    [Fact]
    public void Repos_are_normalized_so_one_repo_is_added_once() =>
        Decider
            .Given<Workspace>(
                Created,
                new WorkspaceEvents.RepoAdded("github.com/acme/payments-api")
            )
            .When(w => WorkspaceDecider.AddRepo(w, "https://GitHub.com/Acme/payments-api.git"))
            .ThenNothing();

    [Fact]
    public void A_recipe_whose_check_has_not_passed_cannot_be_confirmed() =>
        Decider
            .Given<Workspace>(Created, Proposed)
            .When(w => WorkspaceDecider.ConfirmRecipe(w, "h1"))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_failed_check_cannot_be_confirmed() =>
        Decider
            .Given<Workspace>(
                Created,
                Proposed,
                new WorkspaceEvents.RecipeValidated("h1", false, null)
            )
            .When(w => WorkspaceDecider.ConfirmRecipe(w, "h1"))
            .ThenThrows<DomainException>();

    [Fact]
    public void A_passed_check_is_confirmed_and_mining_opens() =>
        Decider
            .Given<Workspace>(
                Created,
                Proposed,
                new WorkspaceEvents.RecipeValidated("h1", true, null)
            )
            .When(w => WorkspaceDecider.ConfirmRecipe(w, "h1"))
            .Then(new WorkspaceEvents.RecipeConfirmed("h1"));

    [Fact]
    public void A_validation_of_an_older_recipe_is_refused() =>
        Decider
            .Given<Workspace>(
                Created,
                Proposed,
                new WorkspaceEvents.RecipeProposed("{\"a\":1}", "h2")
            )
            .When(w => WorkspaceDecider.RecordValidation(w, "h1", true, null))
            .ThenThrows<ConflictException>();

    [Fact]
    public void Only_a_confirmed_recipe_allows_mining()
    {
        var validated = Workspace.Initial with
        {
            Exists = true,
            RecipeHash = "h1",
            RecipeStatus = RecipeStatus.Validated,
        };
        Assert.False(validated.CanMine);
        Assert.True((validated with { RecipeStatus = RecipeStatus.Confirmed }).CanMine);
    }
}

public sealed class EventContractTests
{
    [Fact]
    public void Event_contracts_are_stable() =>
        EventContracts.Verify(
            es => CaseboxStreams.Register(es.Keys(keys => keys.StoreInDatabase())),
            "events.lock"
        );
}
