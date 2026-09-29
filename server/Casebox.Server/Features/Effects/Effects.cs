using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Microsoft.Extensions.Options;

namespace Casebox.Server.Features.Effects;

public sealed record EffectMessage(string Kind, string MessageId, JsonElement Payload, IReadOnlyDictionary<string, string> Headers);

// One kind of side effect. Handlers are idempotent: QueueBox delivers at least once, with any
// concurrency and in any order.
public interface IEffectHandler
{
    string Kind { get; }

    Task HandleAsync(EffectMessage message, CancellationToken ct);
}

public static class EffectEndpoints
{
    public const string TokenHeader = "X-Casebox-Effects-Token";

    // QueueBox delivers every outbox row here, one destination per kind. A non-2xx answer makes
    // QueueBox retry with backoff and dead-letter after its attempts run out.
    public static void MapEffects(this IEndpointRouteBuilder management)
    {
        management.MapPost("/internal/effects/{kind}", async (string kind, HttpContext http, IEnumerable<IEffectHandler> handlers, IOptions<CaseboxOptions> options) =>
        {
            var expected = options.Value.QueueBox.EffectsToken;
            var given = http.Request.Headers[TokenHeader].ToString();
            if (string.IsNullOrEmpty(expected) || !CryptographicOperations.FixedTimeEquals(Encoding.UTF8.GetBytes(given), Encoding.UTF8.GetBytes(expected)))
                return Results.Unauthorized();

            var handler = handlers.FirstOrDefault(h => h.Kind == kind);
            if (handler is null) return Results.NotFound();

            using var document = await JsonDocument.ParseAsync(http.Request.Body, cancellationToken: http.RequestAborted);
            var headers = http.Request.Headers.ToDictionary(h => h.Key, h => h.Value.ToString(), StringComparer.OrdinalIgnoreCase);
            var messageId = headers.GetValueOrDefault("X-Message-Id") ?? "";
            await handler.HandleAsync(new EffectMessage(kind, messageId, document.RootElement.Clone(), headers), http.RequestAborted);
            return Results.NoContent();
        }).ExcludeFromDescription().AllowAnonymous();
    }
}
