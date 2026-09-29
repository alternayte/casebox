using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Effects;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Tokens;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Mvc.Testing;
using Microsoft.AspNetCore.TestHost;
using Microsoft.Extensions.DependencyInjection;

namespace Casebox.Server.Tests.Infrastructure;

// One Casebox server on real Kestrel ports, so QueueBox can reach its management port.
public sealed class CaseboxServer : WebApplicationFactory<Program>
{
    private readonly StackFixture _stack;
    private readonly int _apiPort;

    public CaseboxServer(StackFixture stack, string orgId, int apiPort, int managementPort)
    {
        _stack = stack;
        _apiPort = apiPort;
        OrgId = orgId;
        ManagementPort = managementPort;
        UseKestrel();
    }

    public string OrgId { get; }

    public int ManagementPort { get; }

    public RecordingEffects Effects { get; } = new();

    public void Start() => StartServer();

    protected override void ConfigureWebHost(IWebHostBuilder builder)
    {
        builder.UseSetting("urls", $"http://127.0.0.1:{_apiPort}");
        builder.UseSetting("ConnectionStrings:Casebox", _stack.ConnectionString);
        builder.UseSetting("Casebox:Org:Id", OrgId);
        builder.UseSetting("Casebox:Org:Name", $"Org {OrgId}");
        builder.UseSetting("Casebox:ManagementPort", ManagementPort.ToString(System.Globalization.CultureInfo.InvariantCulture));
        builder.UseSetting("Casebox:Keys:Mode", "database");
        builder.UseSetting("Casebox:LocalAdmin:Password", StackFixture.AdminPassword);
        builder.UseSetting("Casebox:Oidc:Authority", _stack.OidcAuthority);
        builder.UseSetting("Casebox:Oidc:ClientId", "casebox");
        builder.UseSetting("Casebox:Oidc:ClientSecret", "secret");
        builder.UseSetting("Casebox:QueueBox:BaseUrl", _stack.QueueBoxUrl.ToString());
        builder.UseSetting("Casebox:QueueBox:HealthUrl", _stack.QueueBoxHealthUrl.ToString());
        builder.UseSetting("Casebox:QueueBox:PollToken", StackFixture.PollToken);
        builder.UseSetting("Casebox:QueueBox:EffectsToken", StackFixture.EffectsToken);
        builder.UseSetting("Casebox:GitHub:ApiUrl", _stack.Fakes.GitHubApi.ToString());
        builder.ConfigureTestServices(services =>
        {
            services.AddSingleton<IEffectHandler>(Effects);
            services.AddScoped<IInboxHandler, TestInboxHandler>();
            services.AddSingleton<IJobResultHandler, TestJobHandler>();
            services.AddSingleton<IJobResultHandler, IdleJobHandler>();
            services.AddSingleton<Casebox.Server.Features.Privacy.IRosterSource, TestRoster>();
        });
    }

    private readonly CookieContainer _adminCookies = new();
    private readonly SemaphoreSlim _adminLogin = new(1, 1);
    private bool _adminLoggedIn;

    public HttpClient Anonymous() => Client(new CookieContainer());

    private HttpClient Client(CookieContainer cookies) =>
        new(new HttpClientHandler { CookieContainer = cookies, AllowAutoRedirect = false }) { BaseAddress = new Uri($"http://127.0.0.1:{_apiPort}") };

    public HttpClient Management() => new() { BaseAddress = new Uri($"http://127.0.0.1:{ManagementPort}") };

    // A client in the local admin's session (an Owner), with the CSRF header. The admin logs in
    // once per server, because the login endpoint is rate-limited per address.
    public async Task<HttpClient> AdminAsync()
    {
        await _adminLogin.WaitAsync();
        try
        {
            if (!_adminLoggedIn)
            {
                (await Client(_adminCookies).PostAsJsonAsync("/api/v1/auth/local", new { password = StackFixture.AdminPassword })).EnsureSuccessStatusCode();
                _adminLoggedIn = true;
            }
        }
        finally
        {
            _adminLogin.Release();
        }

        // Each client gets its own CSRF cookie; only the session cookie is shared.
        var cookies = new CookieContainer();
        var baseAddress = new Uri($"http://127.0.0.1:{_apiPort}");
        foreach (Cookie cookie in _adminCookies.GetCookies(baseAddress))
            if (cookie.Name == "casebox.session") cookies.Add(baseAddress, cookie);
        var client = Client(cookies);
        await AddCsrfAsync(client);
        return client;
    }

    public static async Task AddCsrfAsync(HttpClient client)
    {
        var csrf = await client.GetFromJsonAsync<AuthEndpoints.Csrf>("/api/v1/auth/csrf");
        client.DefaultRequestHeaders.Remove(AuthSetup.CsrfHeader);
        client.DefaultRequestHeaders.Add(AuthSetup.CsrfHeader, csrf!.Token);
    }

    public async Task<HttpClient> TokenClientAsync(TokenKind kind)
    {
        var admin = await AdminAsync();
        var response = await admin.PostAsJsonAsync("/api/v1/tokens", new { kind = kind.ToString().ToLowerInvariant(), name = $"{kind} {Guid.NewGuid():N}" });
        response.EnsureSuccessStatusCode();
        var issued = await response.Content.ReadFromJsonAsync<IssuedTokenBody>();
        var client = Anonymous();
        client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", issued!.Secret);
        return client;
    }

    public sealed record IssuedTokenBody(TokenBodyInfo Info, string Secret);

    public sealed record TokenBodyInfo(string Id, string Name);
}
