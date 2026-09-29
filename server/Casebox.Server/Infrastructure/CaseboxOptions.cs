namespace Casebox.Server;

// Bound from the "Casebox" configuration section (environment: Casebox__Section__Key).
public sealed class CaseboxOptions
{
    public const string Section = "Casebox";

    public OrgOptions Org { get; set; } = new();
    public LocalAdminOptions LocalAdmin { get; set; } = new();
    public OidcOptions Oidc { get; set; } = new();
    public KeysOptions Keys { get; set; } = new();
    public QueueBoxOptions QueueBox { get; set; } = new();
    public GitHubOptions GitHub { get; set; } = new();

    public sealed class GitHubOptions
    {
        // api.github.com, or a GitHub Enterprise Server's API root such as https://ghe.example.com/api/v3/.
        public Uri ApiUrl { get; set; } = new("https://api.github.com/");
    }

    // `casebox up --demo`: load a synthetic team into an organisation with no session yet.
    public bool Demo { get; set; }

    // The port of /metrics and /healthz. It is never exposed outside the cluster or compose network.
    public int ManagementPort { get; set; } = 8081;

    public sealed class OrgOptions
    {
        // The organisation this server serves. It is created on first start.
        public string Id { get; set; } = "default";
        public string Name { get; set; } = "Casebox";
    }

    public sealed class LocalAdminOptions
    {
        // Set by `casebox up`. Without it, the local admin login is off.
        public string? Password { get; set; }
    }

    public sealed class OidcOptions
    {
        public string? Authority { get; set; }
        public string? ClientId { get; set; }
        public string? ClientSecret { get; set; }

        // OIDC subjects that become Owners on first login. Everyone else starts as a Viewer.
        public List<string> Owners { get; set; } = [];

        public bool Enabled =>
            !string.IsNullOrWhiteSpace(Authority) && !string.IsNullOrWhiteSpace(ClientId);
    }

    public sealed class KeysOptions
    {
        // database | environment | azure. There is no default: the operator chooses.
        public string? Mode { get; set; }
        public string Variable { get; set; } = "DEEDBOX_MASTER_KEY";
        public string? AzureKeyId { get; set; }
    }

    public sealed class QueueBoxOptions
    {
        // QueueBox's data port, where the poll source answers: {BaseUrl}/inbox/poll.
        public Uri BaseUrl { get; set; } = new("http://queuebox:8080");

        // QueueBox's health route; its management port when one is set.
        public Uri HealthUrl { get; set; } = new("http://queuebox:8080/health");

        // The bearer token of the poll source.
        public string? PollToken { get; set; }
    }
}
