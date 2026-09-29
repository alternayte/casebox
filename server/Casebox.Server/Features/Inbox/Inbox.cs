using System.Data.Common;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using Deedbox;
using Microsoft.Extensions.Options;
using Npgsql;
using QueueBox.Inbox;

namespace Casebox.Server.Features.Inbox;

// Handles one event type of one QueueBox source. It appends its Deedbox events through the store
// it is given, which runs in the transaction QueueBox.Inbox opened, so the inbox completion and
// the events commit together or not at all.
public interface IInboxHandler
{
    string Source { get; }

    string EventType { get; }

    Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    );
}

public static class InboxSources
{
    // Fed by the server's own GitHub and Jira pollers. Key: <provider>:<object id>@<updated time>.
    public const string Poll = "poll";
}

// The body the server's pollers post to the poll source. QueueBox reads the key from $.key and
// the event type from $.type; the handler reads the organisation from $.org.
public sealed record PollMessage(string Key, string Type, string Org, object Payload);

// Claims the rows of one pull source and hands each one to the handler for its event type.
public sealed class InboxConsumer(
    string source,
    NpgsqlDataSource dataSource,
    IServiceScopeFactory scopes,
    ILogger<InboxWorker> logger,
    TimeProvider clock
) : BackgroundService
{
    protected override Task ExecuteAsync(CancellationToken stoppingToken)
    {
        var worker = new InboxWorker(
            InboxConnections.From(dataSource),
            new InboxOptions { Source = source },
            logger,
            clock
        );
        return worker.RunAsync(HandleAsync, stoppingToken);
    }

    private async Task HandleAsync(
        InboxMessage message,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        await using var scope = scopes.CreateAsyncScope();
        var handler =
            scope
                .ServiceProvider.GetServices<IInboxHandler>()
                .FirstOrDefault(h => h.Source == message.Source && h.EventType == message.EventType)
            ?? throw new InvalidOperationException(
                $"No inbox handler for source '{message.Source}' and event type '{message.EventType}'."
            );

        var org = message.Payload.TryGetProperty("org", out var o) ? o.GetString() : null;
        if (string.IsNullOrEmpty(org))
            throw new InvalidOperationException(
                $"Inbox message {message.Id} names no organisation."
            );

        var context = scope.ServiceProvider.GetRequiredService<DeedboxContext>();
        context.TenantId = org;
        context.Metadata = new EventMetadata
        {
            CorrelationId = message.CorrelationId ?? message.Id.ToString(),
            Actor = $"inbox:{message.Source}",
        };
        var store = scope
            .ServiceProvider.GetRequiredService<IEventStore>()
            .UseTransaction(transaction);
        await handler.HandleAsync(message, store, transaction, ct);
    }
}

// Posts poll results into QueueBox's poll source. A repeated poll of the same state has the same
// key, so QueueBox stores it once.
public sealed class PollPublisher(HttpClient http, IOptions<CaseboxOptions> options)
{
    public async Task PublishAsync(PollMessage message, CancellationToken ct)
    {
        var queueBox = options.Value.QueueBox;
        using var request = new HttpRequestMessage(
            HttpMethod.Post,
            new Uri(queueBox.BaseUrl, "/inbox/poll")
        )
        {
            Content = JsonContent.Create(message),
        };
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", queueBox.PollToken);
        using var response = await http.SendAsync(request, ct);
        response.EnsureSuccessStatusCode();
    }
}
