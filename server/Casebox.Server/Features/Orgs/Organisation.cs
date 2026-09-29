using Deedbox;

namespace Casebox.Server.Features.Orgs;

public enum PromptMode { Off, Redacted, Full }

public enum PseudonymPeriod { Month, Quarter, Year }

public enum Role { Viewer, Member, Admin, Owner }

public sealed record Budgets(decimal MonthlyUsd, decimal PerEvaluationUsd, decimal ConfirmAboveUsd);

// PromptMode stays null until an admin chooses it: there is no silent default.
public sealed record OrgSettings(PromptMode? PromptMode, int K, PseudonymPeriod PseudonymPeriod, Budgets Budgets)
{
    public const int MinimumK = 2;

    public static OrgSettings Defaults { get; } = new(null, 3, PseudonymPeriod.Quarter, new Budgets(500m, 150m, 50m));
}

public static class OrgEvents
{
    public sealed record Created(string Name, OrgSettings Settings);

    public sealed record SettingsChanged(OrgSettings Settings);

    public sealed record MemberRoleChanged(string AccountId, Role Role);

    public sealed record TokenIssued(string TokenId, string Kind, string Name);

    public sealed record TokenRevoked(string TokenId);
}

// One organisation is one Deedbox tenant, and each tenant holds exactly one org stream.
public sealed record Organisation(bool Exists, string Name, OrgSettings Settings) : IState<Organisation>
{
    public const string StreamId = "org";

    public static Organisation Initial { get; } = new(false, "", OrgSettings.Defaults);

    public static Organisation Evolve(Organisation state, object @event) => @event switch
    {
        OrgEvents.Created e => state with { Exists = true, Name = e.Name, Settings = e.Settings },
        OrgEvents.SettingsChanged e => state with { Settings = e.Settings },
        _ => state,
    };
}

public static class OrgDecider
{
    public static IEnumerable<object> Create(Organisation org, string name)
    {
        if (org.Exists) return [];
        if (string.IsNullOrWhiteSpace(name)) throw new DomainException("An organisation needs a name.");
        return [new OrgEvents.Created(name.Trim(), OrgSettings.Defaults)];
    }

    public static IEnumerable<object> ChangeSettings(Organisation org, OrgSettings settings)
    {
        if (!org.Exists) throw new NotFoundException("The organisation does not exist.");
        if (settings.K < OrgSettings.MinimumK) throw new DomainException($"k must be at least {OrgSettings.MinimumK}.");
        if (settings.Budgets.MonthlyUsd < 0 || settings.Budgets.PerEvaluationUsd < 0 || settings.Budgets.ConfirmAboveUsd < 0)
            throw new DomainException("Budgets cannot be negative.");
        if (org.Settings.PromptMode is not null && settings.PromptMode is null)
            throw new DomainException("The prompt mode cannot be unset once chosen.");
        return settings == org.Settings ? [] : [new OrgEvents.SettingsChanged(settings)];
    }
}
