using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Integrations;

namespace Casebox.Server.Features.AzureDevOps;

// One Azure DevOps Server collection, read with a personal access token over REST API 7.0.
public sealed class AdoClient(HttpClient http, string collection, string token)
{
    public const string ApiVersion = "7.0";

    // Policy evaluations are a preview area on every server version.
    public const string PolicyApiVersion = "7.0-preview.1";

    private readonly Uri _base = new(collection.TrimEnd('/') + "/");

    public string Collection { get; } = collection;

    // A GET relative to the collection URL; api-version 7.0 unless the path names one.
    public async Task<JsonElement> GetAsync(string path, CancellationToken ct)
    {
        var uri = new Uri(_base, WithVersion(path));
        using var request = new HttpRequestMessage(HttpMethod.Get, uri);
        request.Headers.Authorization = new AuthenticationHeaderValue(
            "Basic",
            Convert.ToBase64String(Encoding.ASCII.GetBytes(":" + token))
        );
        request.Headers.Accept.ParseAdd("application/json");
        using var response = await http.SendAsync(request, ct);
        // A refused token gets the sign-in page with 203 or a redirect, not 401, on some servers.
        if (
            !response.IsSuccessStatusCode
            || response.StatusCode == HttpStatusCode.NonAuthoritativeInformation
            || response.Content.Headers.ContentType?.MediaType != "application/json"
        )
            throw new HttpRequestException(
                $"Azure DevOps answered {(int)response.StatusCode} for {uri.AbsolutePath}.",
                null,
                response.IsSuccessStatusCode ? HttpStatusCode.Unauthorized : response.StatusCode
            );
        return (await response.Content.ReadFromJsonAsync<JsonElement>(ct)).Clone();
    }

    // The "value" items of a list, $top at a time, until a page is short, stop says so, or
    // maxPages pages are read. Lists of commits name their paging parameters searchCriteria.$top.
    public async Task<List<JsonElement>> ListAsync(
        string path,
        int maxPages,
        CancellationToken ct,
        Func<JsonElement, bool>? stop = null,
        string prefix = ""
    )
    {
        const int top = 100;
        var items = new List<JsonElement>();
        var separator = path.Contains('?', StringComparison.Ordinal) ? "&" : "?";
        for (var page = 0; page < maxPages; page++)
        {
            var body = await GetAsync(
                $"{path}{separator}{prefix}$top={top}&{prefix}$skip={page * top}",
                ct
            );
            var count = 0;
            foreach (var item in body.GetProperty("value").EnumerateArray())
            {
                count++;
                if (stop?.Invoke(item) == true)
                    return items;
                items.Add(item.Clone());
            }

            if (count < top)
                break;
        }

        return items;
    }

    private static string WithVersion(string path)
    {
        path = path.TrimStart('/');
        if (path.Contains("api-version=", StringComparison.Ordinal))
            return path;
        return path
            + (path.Contains('?', StringComparison.Ordinal) ? "&" : "?")
            + $"api-version={ApiVersion}";
    }
}

// Builds the clients of an organisation's collections from the integration store.
public sealed class AdoClients(IHttpClientFactory http, IntegrationStore integrations)
{
    public const string HttpClientName = "azure-devops";

    public async Task<IReadOnlyDictionary<string, AdoClient>> ForOrgAsync(
        string orgId,
        CancellationToken ct
    )
    {
        var stored = await integrations.GetAsync<AzureDevOpsSettings, AzureDevOpsSecret>(
            orgId,
            CodeHosts.CodeHostKinds.AzureDevOps,
            ct
        );
        if (stored is not { } s)
            return new Dictionary<string, AdoClient>();
        return s
            .Config.Collections.Where(s.Secret.Tokens.ContainsKey)
            .ToDictionary(c => c, c => Create(c, s.Secret.Tokens[c]), StringComparer.Ordinal);
    }

    public AdoClient Create(string collection, string token) =>
        new(http.CreateClient(HttpClientName), collection, token);
}
