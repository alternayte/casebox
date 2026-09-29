using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Capture;

// Native OpenTelemetry from Claude Code and Codex, over OTLP/HTTP with JSON encoding (casebox
// init sets it). Log records become canonical session events; metric data points go to
// casebox.session_metrics. Identity attributes are tokenized or dropped, never stored.
public static class Otlp
{
    public const long SeqBase = 2_000_000_000;

    // Attributes that name a person or an account. The person is tokenized from user.email; the
    // rest are dropped.
    private static readonly HashSet<string> IdentityAttributes =
    [
        "user.email",
        "user.id",
        "user.account_uuid",
        "user.account_id",
        "organization.id",
        "auth.account_id",
    ];

    public static void MapOtlp(this IEndpointRouteBuilder app)
    {
        var otlp = app.MapGroup("/v1").WithTags("Ingest").RequireAuthorization(Policies.Ingest);

        otlp.MapPost(
            "/logs",
            async (HttpContext http, IEventStore store, CaptureStore capture) =>
            {
                if (!IsJson(http))
                    return Results.StatusCode(StatusCodes.Status415UnsupportedMediaType);
                using var body = await JsonDocument.ParseAsync(
                    http.Request.Body,
                    cancellationToken: http.RequestAborted
                );
                var (org, _) = await store.Load<Organisation>(Organisation.StreamId);
                var batches =
                    new Dictionary<string, (CapturedSession Session, List<CapturedEvent> Events)>();

                foreach (var resourceLogs in Array(body.RootElement, "resourceLogs"))
                {
                    var resource = Attributes(
                        resourceLogs.TryGetProperty("resource", out var r) ? r : default
                    );
                    foreach (var scopeLogs in Array(resourceLogs, "scopeLogs"))
                    foreach (var record in Array(scopeLogs, "logRecords"))
                    {
                        var attrs = new Dictionary<string, string>(resource);
                        foreach (var (k, v) in Attributes(record))
                            attrs[k] = v;
                        if (Map(record, attrs) is not { } mapped)
                            continue;

                        var key = mapped.Session.Id;
                        if (!batches.TryGetValue(key, out var batch))
                            batches[key] = batch = (mapped.Session, []);
                        batch.Events.Add(mapped.Event);
                    }
                }

                foreach (var (session, events) in batches.Values)
                foreach (var chunk in events.Chunk(CaptureStore.MaxEventsPerBatch))
                    await capture.StoreAsync(
                        http.User.OrgId(),
                        org.Settings,
                        new CaptureBatch(session, chunk),
                        http.RequestAborted
                    );
                return Results.Ok(new { });
            }
        );

        otlp.MapPost(
            "/metrics",
            async (HttpContext http, NpgsqlDataSource db) =>
            {
                if (!IsJson(http))
                    return Results.StatusCode(StatusCodes.Status415UnsupportedMediaType);
                using var body = await JsonDocument.ParseAsync(
                    http.Request.Body,
                    cancellationToken: http.RequestAborted
                );
                var rows = new List<object>();
                foreach (var resourceMetrics in Array(body.RootElement, "resourceMetrics"))
                {
                    var resource = Attributes(
                        resourceMetrics.TryGetProperty("resource", out var r) ? r : default
                    );
                    foreach (var scopeMetrics in Array(resourceMetrics, "scopeMetrics"))
                    foreach (var metric in Array(scopeMetrics, "metrics"))
                    {
                        var name = metric.TryGetProperty("name", out var n)
                            ? n.GetString() ?? ""
                            : "";
                        foreach (var kind in new[] { "sum", "gauge" })
                        {
                            if (!metric.TryGetProperty(kind, out var data))
                                continue;
                            foreach (var point in Array(data, "dataPoints"))
                            {
                                var attrs = new Dictionary<string, string>(resource);
                                foreach (var (k, v) in Attributes(point))
                                    attrs[k] = v;
                                var agent = AgentOf(name, attrs);
                                if (agent is null)
                                    continue;
                                rows.Add(
                                    new
                                    {
                                        Org = http.User.OrgId(),
                                        Session = SessionIdOf(agent, attrs),
                                        Agent = agent,
                                        Name = name,
                                        Value = point.TryGetProperty("asDouble", out var d)
                                            ? d.GetDouble()
                                        : point.TryGetProperty("asInt", out var i) ? Number(i)
                                        : 0,
                                        At = Time(point, "timeUnixNano"),
                                        Attrs = JsonSerializer.Serialize(Scrub(attrs)),
                                    }
                                );
                            }
                        }
                    }
                }

                if (rows.Count > 0)
                {
                    await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                    await connection.ExecuteAsync(
                        new CommandDefinition(
                            "INSERT INTO casebox.session_metrics (org_id, session_id, agent, name, value, at, attrs) VALUES (@Org, @Session, @Agent, @Name, @Value, @At, @Attrs::jsonb)",
                            rows,
                            cancellationToken: http.RequestAborted
                        )
                    );
                }

                return Results.Ok(new { });
            }
        );
    }

    private sealed record Mapped(CapturedSession Session, CapturedEvent Event);

    // One log record to one canonical event, or null for records Casebox does not use.
    private static Mapped? Map(JsonElement record, Dictionary<string, string> attrs)
    {
        var name =
            attrs.GetValueOrDefault("event.name")
            ?? (
                record.TryGetProperty("body", out var b)
                && b.TryGetProperty("stringValue", out var s)
                    ? s.GetString()
                    : null
            )
            ?? "";
        var agent = AgentOf(name, attrs);
        if (agent is null)
            return null;
        var sessionId = SessionIdOf(agent, attrs);
        if (sessionId is null)
            return null;
        var at = Time(record, "timeUnixNano");
        var eventName = name[(name.LastIndexOf('.') + 1)..];

        CapturedEvent? e = eventName switch
        {
            "user_prompt" => new CapturedEvent(
                0,
                at,
                "prompt",
                attrs.GetValueOrDefault("prompt"),
                null,
                null,
                null
            ),
            "tool_decision" when Rejected(attrs.GetValueOrDefault("decision")) => new CapturedEvent(
                0,
                at,
                "denial",
                null,
                new CapturedTool(attrs.GetValueOrDefault("tool_name"), "denied", null),
                null,
                new Dictionary<string, string>
                {
                    ["denial"] = attrs.GetValueOrDefault("source") ?? "",
                }
            ),
            "tool_result" => new CapturedEvent(
                0,
                at,
                "tool_result",
                null,
                new CapturedTool(
                    attrs.GetValueOrDefault("tool_name"),
                    attrs.GetValueOrDefault("success") == "false" ? "error" : "ok",
                    null
                ),
                null,
                null
            ),
            "api_request" when agent == "claude-code" => new CapturedEvent(
                0,
                at,
                "api_request",
                null,
                null,
                new CapturedUsage(
                    Long(attrs, "input_tokens"),
                    Long(attrs, "output_tokens"),
                    Long(attrs, "cache_read_tokens"),
                    Long(attrs, "cache_creation_tokens"),
                    Decimal(attrs, "cost_usd")
                ),
                new Dictionary<string, string>
                {
                    ["model"] = attrs.GetValueOrDefault("model") ?? "",
                }
            ),
            "sse_event" when attrs.GetValueOrDefault("event.kind") == "response.completed" =>
                new CapturedEvent(
                    0,
                    at,
                    "api_request",
                    null,
                    null,
                    new CapturedUsage(
                        Long(attrs, "input_token_count"),
                        Long(attrs, "output_token_count"),
                        Long(attrs, "cached_token_count"),
                        Long(attrs, "cache_write_token_count"),
                        null
                    ),
                    new Dictionary<string, string>
                    {
                        ["model"] = attrs.GetValueOrDefault("model") ?? "",
                    }
                ),
            "turn_cost" => new CapturedEvent(
                0,
                at,
                "api_request",
                null,
                null,
                new CapturedUsage(null, null, null, null, Decimal(attrs, "usage.estimated_usd")),
                null
            ),
            _ => null,
        };
        if (e is null)
            return null;

        // Claude Code numbers its events per session; Codex does not, so a stable hash of the record
        // keeps a resent batch idempotent.
        var seq = long.TryParse(
            attrs.GetValueOrDefault("event.sequence"),
            NumberStyles.Integer,
            CultureInfo.InvariantCulture,
            out var n
        )
            ? SeqBase + n
            : SeqBase
                + StableHash(
                    $"{at:O}|{name}|{attrs.GetValueOrDefault("call_id")}|{attrs.GetValueOrDefault("turn.id")}"
                ) % 1_000_000_000;

        var email = attrs.GetValueOrDefault("user.email");
        var account =
            attrs.GetValueOrDefault("user.account_uuid")
            ?? attrs.GetValueOrDefault("user.account_id");
        var person =
            !string.IsNullOrEmpty(email) ? $"⟦cbx:email:{email}⟧"
            : !string.IsNullOrEmpty(account) ? $"⟦cbx:account:{agent}/{account}⟧"
            : null;
        if (person is null)
            return null;

        var session = new CapturedSession(
            $"{agent}:{sessionId}",
            agent,
            attrs.GetValueOrDefault("app.version"),
            attrs.GetValueOrDefault("model"),
            null,
            null,
            null,
            null,
            at,
            null,
            "otel",
            null,
            person
        );
        return new Mapped(session, e with { Seq = seq });
    }

    private static bool Rejected(string? decision) =>
        decision is "reject" or "denied" or "abort" or "denied_with_network_policy_deny"
        || (decision?.StartsWith("denied", StringComparison.Ordinal) ?? false);

    private static string? AgentOf(string name, Dictionary<string, string> attrs) =>
        name.StartsWith("claude_code", StringComparison.Ordinal)
        || attrs.ContainsKey("session.id") && !attrs.ContainsKey("conversation.id")
            ? "claude-code"
        : name.StartsWith("codex", StringComparison.Ordinal) || attrs.ContainsKey("conversation.id")
            ? "codex"
        : null;

    private static string? SessionIdOf(string agent, Dictionary<string, string> attrs) =>
        agent == "codex"
            ? attrs.GetValueOrDefault("conversation.id")
            : attrs.GetValueOrDefault("session.id");

    private static Dictionary<string, string> Scrub(Dictionary<string, string> attrs) =>
        attrs
            .Where(a => !IdentityAttributes.Contains(a.Key))
            .ToDictionary(a => a.Key, a => Redaction.Redact(a.Value));

    private static bool IsJson(HttpContext http) =>
        http.Request.ContentType?.StartsWith("application/json", StringComparison.OrdinalIgnoreCase)
        ?? false;

    private static IEnumerable<JsonElement> Array(JsonElement parent, string name) =>
        parent.ValueKind == JsonValueKind.Object
        && parent.TryGetProperty(name, out var list)
        && list.ValueKind == JsonValueKind.Array
            ? list.EnumerateArray()
            : [];

    private static Dictionary<string, string> Attributes(JsonElement parent)
    {
        var result = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var attr in Array(parent, "attributes"))
        {
            if (!attr.TryGetProperty("key", out var k) || !attr.TryGetProperty("value", out var v))
                continue;
            result[k.GetString() ?? ""] = Value(v);
        }

        return result;
    }

    private static string Value(JsonElement v)
    {
        foreach (var p in v.EnumerateObject())
        {
            return p.Name switch
            {
                "stringValue" => p.Value.GetString() ?? "",
                "boolValue" => p.Value.GetBoolean() ? "true" : "false",
                "intValue" => p.Value.ValueKind == JsonValueKind.String
                    ? p.Value.GetString()!
                    : p.Value.GetRawText(),
                "doubleValue" => p.Value.GetRawText(),
                _ => p.Value.GetRawText(),
            };
        }

        return "";
    }

    private static DateTimeOffset Time(JsonElement e, string name)
    {
        if (e.TryGetProperty(name, out var t))
        {
            var text = t.ValueKind == JsonValueKind.String ? t.GetString() : t.GetRawText();
            if (
                long.TryParse(
                    text,
                    NumberStyles.Integer,
                    CultureInfo.InvariantCulture,
                    out var nanos
                )
                && nanos > 0
            )
                return DateTimeOffset.FromUnixTimeMilliseconds(nanos / 1_000_000);
        }

        return DateTimeOffset.UtcNow;
    }

    private static double Number(JsonElement e) =>
        double.Parse(
            e.ValueKind == JsonValueKind.String ? e.GetString()! : e.GetRawText(),
            CultureInfo.InvariantCulture
        );

    private static long? Long(Dictionary<string, string> attrs, string key) =>
        long.TryParse(
            attrs.GetValueOrDefault(key),
            NumberStyles.Integer,
            CultureInfo.InvariantCulture,
            out var v
        )
            ? v
            : null;

    private static decimal? Decimal(Dictionary<string, string> attrs, string key) =>
        decimal.TryParse(
            attrs.GetValueOrDefault(key),
            NumberStyles.Float,
            CultureInfo.InvariantCulture,
            out var v
        )
            ? v
            : null;

    private static long StableHash(string s) =>
        (long)(
            BitConverter.ToUInt64(SHA256.HashData(Encoding.UTF8.GetBytes(s)), 0)
            & 0x7FFFFFFFFFFFFFFF
        );
}
