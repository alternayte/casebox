using System.Net;
using System.Net.Http.Json;
using System.Text.RegularExpressions;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Tests.Infrastructure;

namespace Casebox.Server.Tests.Features;

public sealed partial class AuthTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    [Fact]
    public async Task The_local_admin_logs_in_as_an_Owner()
    {
        var admin = await stack.ServerA.AdminAsync();
        var me = await admin.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);
        Assert.Equal(Role.Owner, me!.Role);
        Assert.Equal(StackFixture.OrgA, me.OrgId);
    }

    [Fact]
    public async Task A_wrong_password_is_refused()
    {
        var client = stack.ServerA.Anonymous();
        var response = await client.PostAsJsonAsync(
            "/api/v1/auth/local",
            new { password = "wrong" },
            Ct
        );
        Assert.Equal(HttpStatusCode.Unauthorized, response.StatusCode);
        Assert.Equal(
            HttpStatusCode.Unauthorized,
            (await client.GetAsync("/api/v1/me", Ct)).StatusCode
        );
    }

    [Fact]
    public async Task A_cookie_request_that_changes_state_needs_the_CSRF_header()
    {
        var admin = await stack.ServerA.AdminAsync();
        admin.DefaultRequestHeaders.Remove(AuthSetup.CsrfHeader);
        var response = await admin.PostAsJsonAsync(
            "/api/v1/workspaces",
            new { name = "csrf-probe" },
            Ct
        );
        Assert.Equal(HttpStatusCode.Forbidden, response.StatusCode);

        await CaseboxServer.AddCsrfAsync(admin);
        Assert.Equal(
            HttpStatusCode.Created,
            (
                await admin.PostAsJsonAsync("/api/v1/workspaces", new { name = "csrf-probe" }, Ct)
            ).StatusCode
        );
    }

    [Fact]
    public async Task An_OIDC_login_creates_a_Viewer_whose_new_role_applies_on_the_next_request()
    {
        var viewer = await OidcLoginAsync();
        var me = await viewer.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);
        Assert.Equal(Role.Viewer, me!.Role);
        Assert.Equal(
            HttpStatusCode.Forbidden,
            (
                await viewer.PostAsJsonAsync(
                    "/api/v1/workspaces",
                    new { name = "viewer-probe" },
                    Ct
                )
            ).StatusCode
        );

        var admin = await stack.ServerA.AdminAsync();
        (
            await admin.PutAsJsonAsync(
                $"/api/v1/accounts/{me.AccountId}/role",
                new { role = "admin" },
                Ct
            )
        ).EnsureSuccessStatusCode();

        Assert.Equal(
            HttpStatusCode.Created,
            (
                await viewer.PostAsJsonAsync(
                    "/api/v1/workspaces",
                    new { name = "viewer-probe" },
                    Ct
                )
            ).StatusCode
        );
    }

    [Fact]
    public async Task Only_an_Admin_changes_privacy_settings_and_k_stays_at_least_two()
    {
        var admin = await stack.ServerA.AdminAsync();
        var org = await admin.GetFromJsonAsync<OrgEndpoints.OrgView>(
            "/api/v1/org",
            Json.Options,
            Ct
        );
        var tooLow = await admin.PutAsJsonAsync(
            "/api/v1/org/settings",
            org!.Settings with
            {
                K = 1,
            },
            Json.Options,
            Ct
        );
        Assert.Equal(HttpStatusCode.UnprocessableEntity, tooLow.StatusCode);

        var viewer = await OidcLoginAsync();
        Assert.Equal(
            HttpStatusCode.Forbidden,
            (
                await viewer.PutAsJsonAsync("/api/v1/org/settings", org.Settings, Json.Options, Ct)
            ).StatusCode
        );
    }

    [Fact]
    public async Task The_last_Owner_cannot_be_demoted()
    {
        var admin = await stack.ServerB.AdminAsync();
        var me = await admin.GetFromJsonAsync<AuthEndpoints.Me>("/api/v1/me", Json.Options, Ct);
        var response = await admin.PutAsJsonAsync(
            $"/api/v1/accounts/{me!.AccountId}/role",
            new { role = "admin" },
            Ct
        );
        Assert.Equal(HttpStatusCode.Conflict, response.StatusCode);
    }

    // Walks the authorization code flow against the mock OIDC provider, which logs in without a form.
    private async Task<HttpClient> OidcLoginAsync()
    {
        var client = stack.ServerA.Anonymous();
        using var browser = new HttpClient(new HttpClientHandler { AllowAutoRedirect = false });

        var challenge = await client.GetAsync("/api/v1/auth/oidc/login", Ct);
        Assert.Equal(HttpStatusCode.Redirect, challenge.StatusCode);
        var authorize = await browser.GetAsync(challenge.Headers.Location, Ct);
        Assert.Equal(HttpStatusCode.Found, authorize.StatusCode);
        var callback = authorize.Headers.Location!;
        Assert.Matches(CallbackPattern(), callback.AbsolutePath);

        var signedIn = await client.GetAsync(callback.PathAndQuery, Ct);
        Assert.Equal(HttpStatusCode.Redirect, signedIn.StatusCode);
        await CaseboxServer.AddCsrfAsync(client);
        return client;
    }

    [GeneratedRegex("/api/v1/auth/oidc/callback$")]
    private static partial Regex CallbackPattern();
}
