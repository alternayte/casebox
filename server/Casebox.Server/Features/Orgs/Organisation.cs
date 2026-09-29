using System.Text.Json.Serialization;
using Deedbox;

namespace Casebox.Server.Features.Orgs;

public enum PromptMode
{
    Off,
    Redacted,
    Full,
}

public enum PseudonymPeriod
{
    Month,
    Quarter,
    Year,
}

public enum Role
{
    Viewer,
    Member,
    Admin,
    Owner,
}

public sealed record Budgets(decimal MonthlyUsd, decimal PerEvaluationUsd, decimal ConfirmAboveUsd);

// PromptMode stays null until an admin chooses it: there is no silent default. The retention
// fields are nullable so events written before they existed still read; null means the default.
public sealed record OrgSettings(
    PromptMode? PromptMode,
    int K,
    PseudonymPeriod PseudonymPeriod,
    Budgets Budgets,
    int? RetentionMonths = null,
    int? TraceRetentionDays = null,
    // The share of the monthly evaluation budget the proposer may spend (SDD section 9).
    decimal? ProposerShare = null
)
{
    public const int MinimumK = 2;
    public const int DefaultRetentionMonths = 12;
    public const int DefaultTraceRetentionDays = 180;
    public const decimal DefaultProposerShare = 0.3m;

    public static OrgSettings Defaults { get; } =
        new(null, 3, PseudonymPeriod.Quarter, new Budgets(500m, 150m, 50m));

    // How long correction text stays linkable: after this, a period's subjects are erased and its
    // pseudonym secret is destroyed.
    [JsonIgnore]
    public int Retention => RetentionMonths ?? DefaultRetentionMonths;

    // How long canonical trace events and telemetry are kept.
    [JsonIgnore]
    public int TraceRetention => TraceRetentionDays ?? DefaultTraceRetentionDays;

    [JsonIgnore]
    public decimal ProposerBudgetShare => ProposerShare ?? DefaultProposerShare;
}

public static class OrgEvents
{
    public sealed record Created(string Name, OrgSettings Settings);

    public sealed record SettingsChanged(OrgSettings Settings);

    public sealed record MemberRoleChanged(string AccountId, Role Role);

    public sealed record TokenIssued(string TokenId, string Kind, string Name);

    public sealed record TokenRevoked(string TokenId);

    // An Admin connected (or removed) an integration. No secret is recorded.
    public sealed record IntegrationConfigured(string Kind, bool Connected);

    // An erasure by identity. The identity itself is never recorded.
    public sealed record ErasurePerformed(int Subjects, int Sessions);

    // A pseudonym period left the retention window: its subjects are erased and its secret destroyed.
    public sealed record PeriodRetired(string Period, int Subjects);
}

// One organisation is one Deedbox tenant, and each tenant holds exactly one org stream.
public sealed record Organisation(bool Exists, string Name, OrgSettings Settings)
    : IState<Organisation>
{
    public const string StreamId = "org";

    public static Organisation Initial { get; } = new(false, "", OrgSettings.Defaults);

    public static Organisation Evolve(Organisation state, object @event) =>
        @event switch
        {
            OrgEvents.Created e => state with
            {
                Exists = true,
                Name = e.Name,
                Settings = e.Settings,
            },
            OrgEvents.SettingsChanged e => state with { Settings = e.Settings },
            _ => state,
        };
}

public static class OrgDecider
{
    public static IEnumerable<object> Create(Organisation org, string name)
    {
        if (org.Exists)
            return [];
        if (string.IsNullOrWhiteSpace(name))
            throw new DomainException("An organisation needs a name.");
        return [new OrgEvents.Created(name.Trim(), OrgSettings.Defaults)];
    }

    public static IEnumerable<object> ChangeSettings(Organisation org, OrgSettings settings)
    {
        if (!org.Exists)
            throw new NotFoundException("The organisation does not exist.");
        if (settings.K < OrgSettings.MinimumK)
            throw new DomainException($"k must be at least {OrgSettings.MinimumK}.");
        if (
            settings.Budgets.MonthlyUsd < 0
            || settings.Budgets.PerEvaluationUsd < 0
            || settings.Budgets.ConfirmAboveUsd < 0
        )
            throw new DomainException("Budgets cannot be negative.");
        if (org.Settings.PromptMode is not null && settings.PromptMode is null)
            throw new DomainException("The prompt mode cannot be unset once chosen.");
        if (settings.RetentionMonths is < 1 or > 120)
            throw new DomainException("The retention window is 1 to 120 months.");
        if (settings.TraceRetentionDays is < 7 or > 3650)
            throw new DomainException("Trace retention is 7 to 3650 days.");
        if (settings.ProposerShare is <= 0 or > 1)
            throw new DomainException(
                "The proposer's share of the monthly budget is above 0 and at most 1."
            );
        return settings == org.Settings ? [] : [new OrgEvents.SettingsChanged(settings)];
    }
}
