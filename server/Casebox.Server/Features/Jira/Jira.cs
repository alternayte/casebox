using System.Data.Common;
using System.Globalization;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text.Json;
using Casebox.Server.Features.Capture;
using Casebox.Server.Features.GitHub;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Integrations;
using Casebox.Server.Features.Orgs;
using Casebox.Server.Features.WorkItems;
using Deedbox;
using QueueBox.Inbox;

namespace Casebox.Server.Features.Jira;

public sealed record JiraIssueSnapshot(
    string Key,
    string Summary,
    string? Description,
    string? Type,
    IReadOnlyList<string> Labels,
    string? Status,
    PersonRef? Assignee,
    DateTimeOffset Created,
    DateTimeOffset? Resolved,
    DateTimeOffset Updated
);

// Polls Jira Data Center every 5 minutes: project in (…) AND updated >= "<cursor>", through the REST
// search API with a personal access token. Requests go only to the configured host.
public sealed class JiraPoller(
    IServiceScopeFactory scopes,
    IntegrationStore integrations,
    IHttpClientFactory http,
    TimeProvider clock,
    ILogger<JiraPoller> logger
) : BackgroundService
{
    public const string MessageType = "jira.issue";
    private const int PageSize = 100;

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            foreach (var org in await integrations.OrgsWithAsync("jira", stoppingToken))
            {
                try
                {
                    await PollAsync(org, stoppingToken);
                }
                catch (Exception e) when (e is not OperationCanceledException)
                {
                    logger.LogWarning(
                        e,
                        "The Jira poll of organisation {Org} failed; it runs again in 5 minutes.",
                        org
                    );
                    await integrations.RecordPollAsync(
                        org,
                        "jira",
                        new { at = clock.GetUtcNow(), error = e.Message },
                        stoppingToken
                    );
                }
            }

            await Task.Delay(CodeHosts.CodeHostPoller.Interval, clock, stoppingToken);
        }
    }

    public async Task<int> PollAsync(string orgId, CancellationToken ct)
    {
        var stored = await integrations.GetAsync<JiraSettings, JiraSecret>(orgId, "jira", ct);
        if (stored is not { } jira)
            return 0;
        await using var scope = scopes.CreateAsyncScope();
        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = orgId;
        context.Metadata = new EventMetadata { Actor = "system:jira" };
        var (org, _) = await scope
            .ServiceProvider.GetRequiredService<IEventStore>()
            .Load<Organisation>(Organisation.StreamId);
        var identities = scope.ServiceProvider.GetRequiredService<Identities>();
        var publisher = scope.ServiceProvider.GetRequiredService<PollPublisher>();

        var cursor = await integrations.CursorAsync(orgId, "jira:search", ct) is { } c
            ? DateTimeOffset.Parse(c, CultureInfo.InvariantCulture)
            : clock.GetUtcNow() - CodeHosts.CodeHostPoller.FirstWindow;
        // Jira compares at minute precision in the server's time zone; a one-minute overlap and the
        // idempotency key keep the edge exact.
        var jql =
            $"project in ({string.Join(',', jira.Config.Projects)}) AND updated >= \"{cursor.AddMinutes(-1).UtcDateTime:yyyy/MM/dd HH:mm}\" ORDER BY updated ASC";
        var baseUri = new Uri(jira.Config.Url);
        var latest = cursor;
        var count = 0;
        for (var start = 0; ; start += PageSize)
        {
            var uri = new Uri(
                baseUri,
                $"rest/api/2/search?jql={Uri.EscapeDataString(jql)}&startAt={start}&maxResults={PageSize}&fields=summary,description,issuetype,labels,status,assignee,created,resolutiondate,updated"
            );
            if (uri.Host != baseUri.Host)
                throw new InvalidOperationException(
                    "Jira requests go only to the configured host."
                );
            using var request = new HttpRequestMessage(HttpMethod.Get, uri);
            request.Headers.Authorization = new AuthenticationHeaderValue(
                "Bearer",
                jira.Secret.Token
            );
            using var response = await http.CreateClient("jira").SendAsync(request, ct);
            response.EnsureSuccessStatusCode();
            var page = await response.Content.ReadFromJsonAsync<JsonElement>(ct);
            var issues = page.GetProperty("issues");
            foreach (var issue in issues.EnumerateArray())
            {
                var snapshot = await SnapshotAsync(issue, identities, org.Settings, ct);
                await publisher.PublishAsync(
                    new PollMessage(
                        $"jira:{snapshot.Key}@{snapshot.Updated:O}",
                        MessageType,
                        orgId,
                        snapshot
                    ),
                    ct
                );
                if (snapshot.Updated > latest)
                    latest = snapshot.Updated;
                count++;
            }

            if (
                issues.GetArrayLength() < PageSize
                || start + PageSize >= page.GetProperty("total").GetInt32()
            )
                break;
        }

        await integrations.SetCursorAsync(orgId, "jira:search", latest.ToString("O"), ct);
        await integrations.RecordPollAsync(
            orgId,
            "jira",
            new { at = clock.GetUtcNow(), issues = count },
            ct
        );
        return count;
    }

    private static async Task<JiraIssueSnapshot> SnapshotAsync(
        JsonElement issue,
        Identities identities,
        OrgSettings settings,
        CancellationToken ct
    )
    {
        var fields = issue.GetProperty("fields");
        var created = Date(fields, "created") ?? DateTimeOffset.UtcNow;
        var period = Identities.PeriodOf(settings.PseudonymPeriod, created);
        PersonRef? assignee = null;
        if (fields.TryGetProperty("assignee", out var a) && a.ValueKind == JsonValueKind.Object)
        {
            // Data Center accounts have a name (the login) and a key; the email, when shown, maps
            // the account to the same person as their git email through the roster.
            var account = Text(a, "name") ?? Text(a, "key") ?? "unknown";
            var (subject, mapped) = await identities.SubjectOfAsync("jira", account, period, ct);
            assignee = new PersonRef(subject, false, mapped);
        }

        return new JiraIssueSnapshot(
            issue.GetProperty("key").GetString()!,
            await identities.TokenizeExternalAsync(Text(fields, "summary") ?? "", period, ct) ?? "",
            await identities.TokenizeExternalAsync(Text(fields, "description"), period, ct),
            fields.TryGetProperty("issuetype", out var t) && t.ValueKind == JsonValueKind.Object
                ? Text(t, "name")
                : null,
            fields.TryGetProperty("labels", out var l) && l.ValueKind == JsonValueKind.Array
                ? l.EnumerateArray().Select(x => x.GetString()!).ToList()
                : [],
            fields.TryGetProperty("status", out var s) && s.ValueKind == JsonValueKind.Object
                ? Text(s, "name")
                : null,
            assignee,
            created,
            Date(fields, "resolutiondate"),
            Date(fields, "updated") ?? created
        );
    }

    private static string? Text(JsonElement e, string property) =>
        e.TryGetProperty(property, out var v) && v.ValueKind == JsonValueKind.String
            ? v.GetString()
            : null;

    // Jira Data Center writes times as 2026-09-29T10:00:00.000+0200, without a colon in the offset.
    private static DateTimeOffset? Date(JsonElement e, string property) =>
        Text(e, property) is { } text
        && DateTimeOffset.TryParse(
            System.Text.RegularExpressions.Regex.Replace(text, @"([+-]\d{2})(\d{2})$", "$1:$2"),
            CultureInfo.InvariantCulture,
            DateTimeStyles.None,
            out var d
        )
            ? d
            : null;
}

public sealed class JiraIssueHandler(Linker linker) : IInboxHandler
{
    public string Source => InboxSources.Poll;

    public string EventType => JiraPoller.MessageType;

    public async Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        var issue = message
            .Payload.GetProperty("payload")
            .Deserialize<JiraIssueSnapshot>(GitHubJson.Options)!;
        var id = WorkItem.JiraStream(issue.Key);
        var snapshot = WorkItemSnapshots.Of(
            issue.Summary,
            issue.Description,
            issue.Type,
            issue.Labels,
            issue.Status,
            issue.Assignee?.Token,
            issue.Resolved is not null
        );
        await store.Execute<WorkItem>(
            id,
            w => WorkItemDecider.Import(w, "jira", issue.Key, null, issue.Created, snapshot)
        );
        await WorkItemSnapshots.LinkWaitingAsync(
            transaction,
            store,
            linker,
            message.Payload.GetProperty("org").GetString()!,
            id,
            null,
            ct
        );
    }
}
