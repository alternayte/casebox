using System.Net.Http.Headers;
using Casebox.Server.Features.Auth;
using Deedbox;

namespace Casebox.Server.Features.Integrations;

public static class IntegrationEndpoints
{
    public sealed record ConnectGitHub(
        string Mode,
        string? Token,
        long? AppId,
        long? InstallationId,
        string? PrivateKey,
        string? WebhookSecret,
        bool Issues = false
    );

    public sealed record ConnectJira(string Url, string Token, IReadOnlyList<string> Projects);

    public sealed record ConnectAzureDevOps(string Url, string Token);

    public static void MapIntegrations(this RouteGroupBuilder api)
    {
        var integrations = api.MapGroup("/integrations")
            .WithTags("Integrations")
            .RequireAuthorization(Policies.Admin);

        integrations.MapGet(
            "/",
            async (HttpContext http, IntegrationStore store) =>
                Results.Ok(await store.ListAsync(http.User.OrgId(), http.RequestAborted))
        );

        // The credential is checked against GitHub before it is stored, so a wrong token fails here
        // and not five minutes later in a poller.
        integrations.MapPut(
            "/github",
            async (
                ConnectGitHub body,
                HttpContext http,
                IntegrationStore store,
                GitHubClients clients,
                IEventStore events
            ) =>
            {
                var (settings, secret) = body.Mode switch
                {
                    "token" when !string.IsNullOrWhiteSpace(body.Token) => (
                        new GitHubSettings("token", Issues: body.Issues),
                        new GitHubSecret(Token: body.Token.Trim())
                    ),
                    "app"
                        when body.AppId is not null
                            && body.InstallationId is not null
                            && !string.IsNullOrWhiteSpace(body.PrivateKey)
                            && !string.IsNullOrWhiteSpace(body.WebhookSecret) => (
                        new GitHubSettings("app", body.AppId, body.InstallationId, body.Issues),
                        new GitHubSecret(
                            PrivateKey: body.PrivateKey,
                            WebhookSecret: body.WebhookSecret
                        )
                    ),
                    _ => throw new DomainException(
                        "Connect GitHub with mode token and a token, or mode app with an App ID, an installation ID, a private key and a webhook secret."
                    ),
                };
                try
                {
                    await clients
                        .Create(settings, secret)
                        .GetAsync("rate_limit", http.RequestAborted);
                }
                catch (HttpRequestException e)
                {
                    throw new DomainException(
                        $"GitHub refused the credential: {e.Message}",
                        Cbx.GitHubRefused
                    );
                }

                await store.SaveAsync(
                    http.User.OrgId(),
                    "github",
                    settings,
                    secret,
                    events,
                    http.RequestAborted
                );
                return Results.NoContent();
            }
        );

        integrations.MapPut(
            "/jira",
            async (
                ConnectJira body,
                HttpContext http,
                IntegrationStore store,
                IHttpClientFactory httpFactory,
                IEventStore events
            ) =>
            {
                if (
                    !Uri.TryCreate(body.Url, UriKind.Absolute, out var url)
                    || url.Scheme is not ("https" or "http")
                )
                    throw new DomainException(
                        "The Jira URL must be an absolute http or https URL."
                    );
                var projects = (body.Projects ?? [])
                    .Select(p => p.Trim().ToUpperInvariant())
                    .Where(p => p.Length > 0)
                    .Distinct()
                    .ToList();
                if (projects.Count == 0 || projects.Any(p => !JiraKeys.ProjectPattern().IsMatch(p)))
                    throw new DomainException("Name at least one Jira project key, such as PAY.");
                if (string.IsNullOrWhiteSpace(body.Token))
                    throw new DomainException("A Jira personal access token is required.");

                using var request = new HttpRequestMessage(
                    HttpMethod.Get,
                    new Uri(url, "rest/api/2/myself")
                );
                request.Headers.Authorization = new AuthenticationHeaderValue(
                    "Bearer",
                    body.Token.Trim()
                );
                using var response = await httpFactory
                    .CreateClient("jira")
                    .SendAsync(request, http.RequestAborted);
                if (!response.IsSuccessStatusCode)
                    throw new DomainException(
                        $"Jira refused the token ({(int)response.StatusCode}).",
                        Cbx.JiraRefused
                    );

                await store.SaveAsync(
                    http.User.OrgId(),
                    "jira",
                    new JiraSettings(url.GetLeftPart(UriPartial.Path).TrimEnd('/') + "/", projects),
                    new JiraSecret(body.Token.Trim()),
                    events,
                    http.RequestAborted
                );
                return Results.NoContent();
            }
        );

        // One collection and its personal access token; other collections stay as they are. The token
        // is checked against the collection before it is stored.
        integrations.MapPut(
            "/azure-devops",
            async (
                ConnectAzureDevOps body,
                HttpContext http,
                IntegrationStore store,
                AzureDevOps.AdoClients clients,
                IEventStore events
            ) =>
            {
                var collection =
                    CodeHosts.RepoHosts.NormalizeCollection(body.Url)
                    ?? throw new DomainException(
                        "The collection URL must be an http or https URL with its path, such as https://ado.example.com/tfs/DefaultCollection."
                    );
                if (string.IsNullOrWhiteSpace(body.Token))
                    throw new DomainException("An Azure DevOps personal access token is required.");
                var token = body.Token.Trim();
                try
                {
                    await clients
                        .Create(collection, token)
                        .GetAsync("_apis/projects?$top=1", http.RequestAborted);
                }
                catch (HttpRequestException e)
                {
                    throw new DomainException(
                        $"Azure DevOps refused the token: {e.Message}",
                        Cbx.AzureDevOpsRefused
                    );
                }

                var org = http.User.OrgId();
                var current = await store.GetAsync<AzureDevOpsSettings, AzureDevOpsSecret>(
                    org,
                    CodeHosts.CodeHostKinds.AzureDevOps,
                    http.RequestAborted
                );
                var tokens = new Dictionary<string, string>(
                    current?.Secret.Tokens ?? new Dictionary<string, string>(),
                    StringComparer.Ordinal
                )
                {
                    [collection] = token,
                };
                await store.SaveAsync(
                    org,
                    CodeHosts.CodeHostKinds.AzureDevOps,
                    new AzureDevOpsSettings([.. tokens.Keys.Order(StringComparer.Ordinal)]),
                    new AzureDevOpsSecret(tokens),
                    events,
                    http.RequestAborted
                );
                return Results.NoContent();
            }
        );

        integrations.MapDelete(
            "/{kind}",
            async (string kind, HttpContext http, IntegrationStore store, IEventStore events) =>
            {
                if (kind is not ("github" or "jira" or CodeHosts.CodeHostKinds.AzureDevOps))
                    return Results.NotFound();
                await store.RemoveAsync(http.User.OrgId(), kind, events, http.RequestAborted);
                return Results.NoContent();
            }
        );
    }
}

public sealed record JiraSecret(string Token);

public static partial class JiraKeys
{
    [System.Text.RegularExpressions.GeneratedRegex("^[A-Z][A-Z0-9_]{1,9}$")]
    public static partial System.Text.RegularExpressions.Regex ProjectPattern();
}
