using System.Text.Json;
using System.Text.Json.Serialization;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Capture;

// The canonical session format of docs/specs/capture.md.
public sealed record CapturedSession(
    string Id,
    string Agent,
    string? AgentVersion,
    string? Model,
    string? Repo,
    string? Branch,
    string? HeadStart,
    string? HeadEnd,
    DateTimeOffset StartedAt,
    DateTimeOffset? EndedAt,
    string Source,
    string? WorkItem,
    string Person);

public sealed record CapturedTool(string? Name, string? Status, IReadOnlyList<string>? Files);

public sealed record CapturedUsage(long? InputTokens, long? OutputTokens, long? CacheReadTokens, long? CacheWriteTokens, decimal? CostUsd);

public sealed record CapturedEvent(
    long Seq,
    DateTimeOffset At,
    string Kind,
    string? Text,
    CapturedTool? Tool,
    CapturedUsage? Usage,
    IReadOnlyDictionary<string, string>? Attrs);

public sealed record CaptureBatch(CapturedSession Session, IReadOnlyList<CapturedEvent> Events);

public sealed record CaptureConfig(PromptMode? PromptMode, bool CaptureAllowed);

// Redacts, tokenizes and stores one batch. The batch endpoint and the OTLP ingest both use it,
// so every source obeys the same prompt mode and no identity is ever written.
public sealed class CaptureStore(Identities identities, NpgsqlDataSource db, TimeProvider clock)
{
    public const int MaxEventsPerBatch = 2000;

    private static readonly HashSet<string> Kinds =
    [
        "session_start", "prompt", "response", "tool_call", "tool_result", "interruption", "denial",
        "rewind", "human_edit", "compaction", "session_end", "api_request", "unknown",
    ];

    // Captured directly: Claude Code, Codex, Cursor CLI. Through Entire checkpoints also: OpenCode, Pi, Copilot CLI.
    private static readonly HashSet<string> Agents = ["claude-code", "codex", "cursor-cli", "opencode", "pi", "copilot-cli"];

    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web) { DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull };

    public async Task<int> StoreAsync(string orgId, OrgSettings settings, CaptureBatch batch, CancellationToken ct)
    {
        if (settings.PromptMode is not { } mode)
            throw new ConflictException("Capture is off until an admin chooses a prompt mode (casebox init).");
        Validate(batch);

        var session = batch.Session;
        var sessionPeriod = Identities.PeriodOf(settings.PseudonymPeriod, session.StartedAt);
        var (person, mapped) = await identities.SubjectOfMarkAsync(session.Person, sessionPeriod, ct)
            ?? throw new DomainException("The session's person must be one identity mark.");

        var rows = new List<object>(batch.Events.Count);
        foreach (var e in batch.Events)
        {
            var period = Identities.PeriodOf(settings.PseudonymPeriod, e.At);
            var text = Keep(mode, e.Kind) && e.Text is { } t ? await identities.TokenizeAsync(Redaction.Redact(t), period, ct) : null;
            var attrs = new Dictionary<string, string>();
            foreach (var (key, value) in e.Attrs ?? new Dictionary<string, string>())
                attrs[key] = await identities.TokenizeAsync(Redaction.Redact(value), period, ct);
            var tool = e.Tool is null ? null : e.Tool with { Files = e.Tool.Files?.Select(Redaction.Redact).ToList() };
            rows.Add(new
            {
                Org = orgId,
                Session = session.Id,
                e.Seq,
                e.At,
                e.Kind,
                Text = text,
                Tool = tool is null ? null : JsonSerializer.Serialize(tool, Json),
                Usage = e.Usage is null ? null : JsonSerializer.Serialize(e.Usage, Json),
                Attrs = JsonSerializer.Serialize(attrs, Json),
            });
        }

        await using var connection = await db.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);
        foreach (var month in batch.Events.Select(e => new DateTime(e.At.UtcDateTime.Year, e.At.UtcDateTime.Month, 1, 0, 0, 0, DateTimeKind.Utc)).Distinct())
            await EnsurePartitionAsync(connection, transaction, month, ct);

        var now = clock.GetUtcNow();
        await connection.ExecuteAsync(new CommandDefinition(
            """
            INSERT INTO casebox.sessions (org_id, id, agent, agent_version, model, repo, branch, head_start, head_end, person, person_mapped, period, work_item, source, started_at, ended_at, created_at, updated_at)
            VALUES (@Org, @Id, @Agent, @AgentVersion, @Model, @Repo, @Branch, @HeadStart, @HeadEnd, @Person, @Mapped, @Period, @WorkItem, @Source, @StartedAt, @EndedAt, @Now, @Now)
            ON CONFLICT (org_id, id) DO UPDATE SET
                agent_version = COALESCE(EXCLUDED.agent_version, sessions.agent_version),
                model = COALESCE(EXCLUDED.model, sessions.model),
                repo = COALESCE(sessions.repo, EXCLUDED.repo),
                branch = COALESCE(sessions.branch, EXCLUDED.branch),
                head_start = COALESCE(sessions.head_start, EXCLUDED.head_start),
                head_end = COALESCE(EXCLUDED.head_end, sessions.head_end),
                work_item = COALESCE(EXCLUDED.work_item, sessions.work_item),
                person = CASE WHEN EXCLUDED.person_mapped AND NOT sessions.person_mapped THEN EXCLUDED.person ELSE sessions.person END,
                person_mapped = sessions.person_mapped OR EXCLUDED.person_mapped,
                started_at = LEAST(sessions.started_at, EXCLUDED.started_at),
                ended_at = GREATEST(sessions.ended_at, EXCLUDED.ended_at),
                updated_at = EXCLUDED.updated_at
            """,
            new
            {
                Org = orgId, session.Id, session.Agent, session.AgentVersion, session.Model, Repo = session.Repo?.ToLowerInvariant(), session.Branch,
                session.HeadStart, session.HeadEnd, Person = person, Mapped = mapped, Period = sessionPeriod, session.WorkItem, session.Source, session.StartedAt, session.EndedAt, Now = now,
            },
            transaction, cancellationToken: ct));

        var inserted = await connection.ExecuteAsync(new CommandDefinition(
            """
            INSERT INTO casebox.session_events (org_id, session_id, seq, at, kind, text, tool, usage, attrs)
            VALUES (@Org, @Session, @Seq, @At, @Kind, @Text, @Tool::jsonb, @Usage::jsonb, @Attrs::jsonb)
            ON CONFLICT DO NOTHING
            """,
            rows, transaction, cancellationToken: ct));
        await connection.ExecuteAsync(new CommandDefinition(
            "UPDATE casebox.sessions SET event_count = event_count + @Inserted WHERE org_id = @Org AND id = @Id",
            new { Inserted = inserted, Org = orgId, session.Id }, transaction, cancellationToken: ct));
        await transaction.CommitAsync(ct);
        return inserted;
    }

    // Prompt mode off keeps structure only; redacted keeps prompts and responses but not tool
    // output; full keeps everything.
    private static bool Keep(PromptMode mode, string kind) => mode switch
    {
        PromptMode.Off => false,
        PromptMode.Redacted => kind is not ("tool_result" or "tool_call"),
        _ => true,
    };

    private static void Validate(CaptureBatch batch)
    {
        var s = batch.Session;
        if (string.IsNullOrWhiteSpace(s.Id) || s.Id.Length > 200 || !s.Id.StartsWith($"{s.Agent}:", StringComparison.Ordinal))
            throw new DomainException("A session ID is '<agent>:<native id>', at most 200 characters.");
        if (!Agents.Contains(s.Agent)) throw new DomainException($"Unknown agent '{s.Agent}'.");
        if (s.Source is not ("import" or "hook" or "otel" or "entire")) throw new DomainException($"Unknown source '{s.Source}'.");
        if (batch.Events.Count > MaxEventsPerBatch) throw new DomainException($"A batch holds at most {MaxEventsPerBatch} events.");
        foreach (var e in batch.Events)
        {
            if (!Kinds.Contains(e.Kind)) throw new DomainException($"Unknown event kind '{e.Kind}'.");
            if (e.Seq < 0) throw new DomainException("An event sequence number is never negative.");
        }
    }

    private static Task EnsurePartitionAsync(NpgsqlConnection connection, NpgsqlTransaction transaction, DateTime month, CancellationToken ct)
    {
        var name = $"session_events_{month:yyyy_MM}";
        var next = month.AddMonths(1);
        // The advisory lock serializes concurrent first writers of a month.
        return connection.ExecuteAsync(new CommandDefinition(
            $"SELECT pg_advisory_xact_lock(hashtext('{name}')); CREATE TABLE IF NOT EXISTS casebox.{name} PARTITION OF casebox.session_events FOR VALUES FROM ('{month:yyyy-MM-dd}') TO ('{next:yyyy-MM-dd}')",
            transaction: transaction, cancellationToken: ct));
    }
}

public static class Ingest
{
    public static void MapIngest(this IEndpointRouteBuilder app)
    {
        var ingest = app.MapGroup("/ingest/v1").WithTags("Ingest").RequireAuthorization(Policies.Ingest);

        ingest.MapGet("/config", async (IEventStore store) =>
        {
            var (org, _) = await store.Load<Organisation>(Organisation.StreamId);
            return Results.Ok(new CaptureConfig(org.Settings.PromptMode, org.Settings.PromptMode is not null));
        });

        ingest.MapPost("/sessions", async (CaptureBatch batch, HttpContext http, IEventStore store, CaptureStore capture, WorkItems.Linker linker) =>
        {
            var (org, _) = await store.Load<Organisation>(Organisation.StreamId);
            var stored = await capture.StoreAsync(http.User.OrgId(), org.Settings, batch, http.RequestAborted);
            await linker.LinkSessionAsync(batch.Session.Id, http.RequestAborted);
            return Results.Ok(new { accepted = batch.Events.Count, stored });
        });
    }
}
