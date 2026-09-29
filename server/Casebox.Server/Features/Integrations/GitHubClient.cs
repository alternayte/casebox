using System.Collections.Concurrent;
using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Microsoft.Extensions.Options;

namespace Casebox.Server.Features.Integrations;

// GitHub ran out of requests for this credential; the poll continues after the reset.
public sealed class GitHubRateLimitedException(DateTimeOffset reset)
    : Exception($"GitHub's rate limit is used up until {reset:O}.")
{
    public DateTimeOffset Reset { get; } = reset;
}

// One organisation's GitHub access: a fine-grained token, or a GitHub App installation token that
// is minted from the App's private key and cached until shortly before it expires.
public sealed class GitHubClient(
    HttpClient http,
    Uri api,
    Func<CancellationToken, Task<string>> token
)
{
    public static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);

    public async Task<JsonElement> GetAsync(string path, CancellationToken ct)
    {
        using var response = await SendAsync(HttpMethod.Get, path, null, ct);
        return (await response.Content.ReadFromJsonAsync<JsonElement>(Json, ct)).Clone();
    }

    // Every page of a list, following the Link header; stops early when stop says so.
    public async Task<List<JsonElement>> ListAsync(
        string path,
        Func<JsonElement, bool>? stop,
        int maxPages,
        CancellationToken ct
    )
    {
        var items = new List<JsonElement>();
        var next =
            path + (path.Contains('?', StringComparison.Ordinal) ? "&" : "?") + "per_page=100";
        for (var page = 0; next is not null && page < maxPages; page++)
        {
            using var response = await SendAsync(HttpMethod.Get, next, null, ct);
            var body = await response.Content.ReadFromJsonAsync<JsonElement>(Json, ct);
            foreach (var item in body.EnumerateArray())
            {
                if (stop?.Invoke(item) == true)
                    return items;
                items.Add(item.Clone());
            }

            next = NextLink(response);
        }

        return items;
    }

    // A write: POST, PATCH or PUT with a JSON body; the answer's JSON.
    public async Task<JsonElement> SendJsonAsync(
        HttpMethod method,
        string path,
        object body,
        CancellationToken ct
    )
    {
        using var response = await SendAsync(method, path, body, ct);
        return (await response.Content.ReadFromJsonAsync<JsonElement>(Json, ct)).Clone();
    }

    public async Task<JsonElement> GraphQLAsync(
        string query,
        object variables,
        CancellationToken ct
    )
    {
        using var response = await SendAsync(
            HttpMethod.Post,
            "graphql",
            new { query, variables },
            ct
        );
        var body = await response.Content.ReadFromJsonAsync<JsonElement>(Json, ct);
        if (body.TryGetProperty("errors", out var errors) && errors.GetArrayLength() > 0)
            throw new HttpRequestException(
                $"GitHub GraphQL: {errors[0].GetProperty("message").GetString()}"
            );
        return body.GetProperty("data").Clone();
    }

    private async Task<HttpResponseMessage> SendAsync(
        HttpMethod method,
        string path,
        object? body,
        CancellationToken ct
    )
    {
        var uri = path.StartsWith("http", StringComparison.Ordinal)
            ? new Uri(path)
            : new Uri(api, path.TrimStart('/'));
        if (uri.Host != api.Host)
            throw new InvalidOperationException("GitHub pagination pointed at another host.");
        using var request = new HttpRequestMessage(method, uri);
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", await token(ct));
        request.Headers.Accept.ParseAdd("application/vnd.github+json");
        request.Headers.Add("X-GitHub-Api-Version", "2022-11-28");
        request.Headers.UserAgent.ParseAdd("casebox");
        if (body is not null)
            request.Content = JsonContent.Create(body, options: Json);

        var response = await http.SendAsync(request, ct);
        if (
            response.StatusCode is HttpStatusCode.Forbidden or HttpStatusCode.TooManyRequests
            && response.Headers.TryGetValues("x-ratelimit-remaining", out var remaining)
            && remaining.FirstOrDefault() == "0"
        )
        {
            var reset =
                response.Headers.TryGetValues("x-ratelimit-reset", out var r)
                && long.TryParse(r.FirstOrDefault(), out var epoch)
                    ? DateTimeOffset.FromUnixTimeSeconds(epoch)
                    : DateTimeOffset.UtcNow.AddMinutes(5);
            response.Dispose();
            throw new GitHubRateLimitedException(reset);
        }

        if (!response.IsSuccessStatusCode)
        {
            var status = response.StatusCode;
            response.Dispose();
            throw new HttpRequestException(
                $"GitHub answered {(int)status} for {uri.AbsolutePath}.",
                null,
                status
            );
        }

        return response;
    }

    private static string? NextLink(HttpResponseMessage response)
    {
        if (!response.Headers.TryGetValues("Link", out var links))
            return null;
        foreach (var part in string.Join(",", links).Split(','))
        {
            var sections = part.Split(';');
            if (sections.Length == 2 && sections[1].Trim() == "rel=\"next\"")
                return sections[0].Trim().Trim('<', '>');
        }

        return null;
    }
}

// Builds the GitHub client for an organisation from its stored settings.
public sealed class GitHubClients(
    IHttpClientFactory http,
    IntegrationStore integrations,
    IOptions<CaseboxOptions> options,
    TimeProvider clock
)
{
    private readonly ConcurrentDictionary<
        long,
        (string Token, DateTimeOffset Expires)
    > _installationTokens = new();

    public Uri Api => options.Value.GitHub.ApiUrl;

    public async Task<GitHubClient?> ForOrgAsync(string orgId, CancellationToken ct)
    {
        var stored = await integrations.GetAsync<GitHubSettings, GitHubSecret>(orgId, "github", ct);
        if (stored is not { } s)
            return null;
        return Create(s.Config, s.Secret);
    }

    public GitHubClient Create(GitHubSettings settings, GitHubSecret secret) =>
        settings.Mode == "app"
            ? new GitHubClient(
                http.CreateClient("github"),
                Api,
                ct => InstallationTokenAsync(settings, secret, ct)
            )
            : new GitHubClient(
                http.CreateClient("github"),
                Api,
                _ =>
                    Task.FromResult(
                        secret.Token
                            ?? throw new InvalidOperationException("The GitHub token is missing.")
                    )
            );

    private async Task<string> InstallationTokenAsync(
        GitHubSettings settings,
        GitHubSecret secret,
        CancellationToken ct
    )
    {
        var installation =
            settings.InstallationId
            ?? throw new InvalidOperationException("The GitHub App installation ID is missing.");
        if (
            _installationTokens.TryGetValue(installation, out var cached)
            && cached.Expires > clock.GetUtcNow().AddMinutes(5)
        )
            return cached.Token;

        var jwt = AppJwt(
            settings.AppId ?? throw new InvalidOperationException("The GitHub App ID is missing."),
            secret.PrivateKey!
        );
        using var request = new HttpRequestMessage(
            HttpMethod.Post,
            new Uri(Api, $"app/installations/{installation}/access_tokens")
        );
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", jwt);
        request.Headers.Accept.ParseAdd("application/vnd.github+json");
        request.Headers.UserAgent.ParseAdd("casebox");
        using var response = await http.CreateClient("github").SendAsync(request, ct);
        response.EnsureSuccessStatusCode();
        var body = await response.Content.ReadFromJsonAsync<JsonElement>(ct);
        var token = body.GetProperty("token").GetString()!;
        _installationTokens[installation] = (
            token,
            body.GetProperty("expires_at").GetDateTimeOffset()
        );
        return token;
    }

    // A GitHub App authenticates with an RS256 JWT that lives at most ten minutes.
    private string AppJwt(long appId, string privateKeyPem)
    {
        static string B64(byte[] b) =>
            Convert.ToBase64String(b).TrimEnd('=').Replace('+', '-').Replace('/', '_');
        var now = clock.GetUtcNow().ToUnixTimeSeconds();
        var header = B64(Encoding.UTF8.GetBytes("""{"alg":"RS256","typ":"JWT"}"""));
        var payload = B64(
            JsonSerializer.SerializeToUtf8Bytes(
                new
                {
                    iat = now - 60,
                    exp = now + 540,
                    iss = appId.ToString(System.Globalization.CultureInfo.InvariantCulture),
                }
            )
        );
        using var rsa = RSA.Create();
        rsa.ImportFromPem(privateKeyPem);
        var signature = rsa.SignData(
            Encoding.ASCII.GetBytes($"{header}.{payload}"),
            HashAlgorithmName.SHA256,
            RSASignaturePadding.Pkcs1
        );
        return $"{header}.{payload}.{B64(signature)}";
    }
}
