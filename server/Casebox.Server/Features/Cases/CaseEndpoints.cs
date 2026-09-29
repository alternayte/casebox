using System.Text.Json;
using Casebox.Server.Features.Auth;
using Casebox.Server.Features.Privacy;
using Casebox.Server.Features.Steering;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Cases;

// Review in the UI and the CLI (docs/specs/cases.md, Review). Instructions leave the API masked.
public static class CaseEndpoints
{
    public sealed record CaseSummary(
        string Id,
        string Kind,
        string Workspace,
        string Status,
        string Scope,
        string Source,
        string? WorkItem,
        int Rank,
        int? FailToPass,
        int? PassToPass,
        bool Drift,
        double Weight,
        string? Split,
        bool HasInstruction,
        DateTimeOffset MinedAt,
        DateTimeOffset UpdatedAt
    );

    public sealed record CaseView(
        CaseSummary Summary,
        JsonElement Repos,
        string? Instruction,
        JsonElement Signatures,
        JsonElement Assertions,
        JsonElement Judge,
        bool AssertionsApproved,
        string? Oracle,
        double? Seconds,
        string? FailureReason,
        string? FailureDetail,
        string? RejectReason,
        string? RetiredReason,
        string RecipeHash,
        string HarnessHash,
        string? ApprovalBlocker
    );

    public sealed record ValidationRun(
        DateTimeOffset At,
        bool Passed,
        string? Reason,
        string? Detail,
        int? FailToPass,
        int? PassToPass,
        double? Seconds
    );

    public sealed record InstructionBody(string Text);

    public sealed record RejectionBody(string Reason);

    public sealed record AssertionsBody(
        IReadOnlyList<Assertion> Assertions,
        IReadOnlyList<JudgeQuestion>? Judge
    );

    public sealed record BulkApproval(IReadOnlyList<string> Ids);

    public static void MapCases(this RouteGroupBuilder api)
    {
        var cases = api.MapGroup("/cases").WithTags("Cases").RequireAuthorization(Policies.Viewer);

        cases.MapGet(
            "/",
            async (
                string? status,
                string? kind,
                string? workspace,
                string? split,
                int? limit,
                int? offset,
                HttpContext http,
                NpgsqlDataSource db
            ) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var rows = await connection.QueryAsync<Row>(
                    new CommandDefinition(
                        $"""
                        SELECT {Columns} FROM casebox.case_catalog
                        WHERE org_id = @Org AND (@Status::text IS NULL OR status = @Status) AND (@Kind::text IS NULL OR kind = @Kind)
                          AND (@Workspace::text IS NULL OR workspace = @Workspace) AND (@Split::text IS NULL OR split = @Split)
                        ORDER BY rank DESC, mined_at DESC, id LIMIT @Limit OFFSET @Offset
                        """,
                        new
                        {
                            Org = http.User.OrgId(),
                            Status = status,
                            Kind = kind,
                            Workspace = workspace,
                            Split = split,
                            Limit = Math.Clamp(limit ?? 200, 1, 1000),
                            Offset = Math.Max(offset ?? 0, 0),
                        },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(rows.Select(r => r.Summary()).ToList());
            }
        );

        // The review queue: validated cases with an instruction, waiting for a person.
        cases.MapGet(
            "/queue",
            async (string? workspace, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var rows = await connection.QueryAsync<Row>(
                    new CommandDefinition(
                        $"""
                        SELECT {Columns} FROM casebox.case_catalog
                        WHERE org_id = @Org AND status = 'validated' AND instruction IS NOT NULL AND (@Workspace::text IS NULL OR workspace = @Workspace)
                        ORDER BY rank DESC, mined_at DESC, id
                        """,
                        new { Org = http.User.OrgId(), Workspace = workspace },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(rows.Select(r => r.View()).ToList());
            }
        );

        cases.MapGet(
            "/{id}",
            async (string id, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var row = await connection.QuerySingleOrDefaultAsync<Row>(
                    new CommandDefinition(
                        $"SELECT {Columns} FROM casebox.case_catalog WHERE org_id = @Org AND id = @Id",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                return row is null ? Results.NotFound() : Results.Ok(row.View());
            }
        );

        cases.MapGet(
            "/{id}/validations",
            async (string id, HttpContext http, NpgsqlDataSource db) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var runs = await connection.QueryAsync<(
                    DateTime At,
                    bool Passed,
                    string? Reason,
                    string? Detail,
                    int? FailToPass,
                    int? PassToPass,
                    double? Seconds
                )>(
                    new CommandDefinition(
                        "SELECT at, passed, reason, detail, fail_to_pass, pass_to_pass, seconds FROM casebox.case_validations WHERE org_id = @Org AND case_id = @Id ORDER BY at DESC",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                return Results.Ok(
                    runs.Select(r => new ValidationRun(
                            new DateTimeOffset(DateTime.SpecifyKind(r.At, DateTimeKind.Utc)),
                            r.Passed,
                            r.Reason,
                            Masking.Mask(r.Detail),
                            r.FailToPass,
                            r.PassToPass,
                            r.Seconds
                        ))
                        .ToList()
                );
            }
        );

        // The oracle a validation recorded: which tests decide the case.
        cases.MapGet(
            "/{id}/oracle",
            async (string id, HttpContext http, NpgsqlDataSource db, Blobs.BlobStore blobs) =>
            {
                await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
                var oracle = await connection.QuerySingleOrDefaultAsync<string?>(
                    new CommandDefinition(
                        "SELECT oracle FROM casebox.case_catalog WHERE org_id = @Org AND id = @Id",
                        new { Org = http.User.OrgId(), Id = id },
                        cancellationToken: http.RequestAborted
                    )
                );
                if (oracle is null)
                    return Results.NotFound();
                var blob = await blobs.GetAsync(http.User.OrgId(), oracle, http.RequestAborted);
                if (blob is null)
                    return Results.NotFound();
                var doc = JsonDocument.Parse(blob.Data).RootElement;
                JsonElement? Field(string name) =>
                    doc.TryGetProperty(name, out var v) ? v.Clone() : null;
                return Results.Ok(
                    new
                    {
                        tests = Field("tests"),
                        testFiles = Field("testFiles"),
                        commands = Field("commands"),
                    }
                );
            }
        );

        cases
            .MapPut(
                "/{id}/instruction",
                async (
                    string id,
                    InstructionBody body,
                    HttpContext http,
                    IEventStore store,
                    Capture.Identities identities
                ) =>
                {
                    // A person's edit is tokenized like any text from outside before it is stored.
                    var (org, _) = await store.Load<Orgs.Organisation>(
                        Orgs.Organisation.StreamId,
                        http.RequestAborted
                    );
                    var text = await identities.TokenizeExternalAsync(
                        body.Text ?? "",
                        Capture.Identities.PeriodOf(
                            org.Settings.PseudonymPeriod,
                            DateTimeOffset.UtcNow
                        ),
                        http.RequestAborted
                    );
                    return await Execute(
                        store,
                        id,
                        c => CaseDecider.EditInstruction(c, text ?? "", Account(http)),
                        http
                    );
                }
            )
            .RequireAuthorization(Policies.Member);

        cases
            .MapPost(
                "/{id}/assertions",
                (string id, AssertionsBody body, HttpContext http, IEventStore store) =>
                    Execute(
                        store,
                        id,
                        c =>
                            CaseDecider.ApproveAssertions(
                                c,
                                body.Assertions ?? [],
                                body.Judge ?? [],
                                Account(http)
                            ),
                        http
                    )
            )
            .RequireAuthorization(Policies.Member);

        cases
            .MapPost(
                "/{id}/approval",
                async (string id, HttpContext http, IEventStore store, NpgsqlDataSource db) =>
                {
                    await ApproveAsync(store, db, http, id);
                    return Results.NoContent();
                }
            )
            .RequireAuthorization(Policies.Member);

        // Bulk approve: each case on its own, so one that is not ready does not hold back the rest.
        cases
            .MapPost(
                "/approvals",
                async (
                    BulkApproval body,
                    HttpContext http,
                    IEventStore store,
                    NpgsqlDataSource db
                ) =>
                {
                    var approved = new List<string>();
                    var refused = new List<object>();
                    foreach (var id in (body.Ids ?? []).Distinct(StringComparer.Ordinal))
                    {
                        try
                        {
                            await ApproveAsync(store, db, http, id);
                            approved.Add(id);
                        }
                        catch (DomainException e)
                        {
                            refused.Add(new { id, reason = e.Message });
                        }
                    }

                    return Results.Ok(new { approved, refused });
                }
            )
            .RequireAuthorization(Policies.Member);

        cases
            .MapPost(
                "/{id}/rejection",
                (string id, RejectionBody body, HttpContext http, IEventStore store) =>
                    Execute(
                        store,
                        id,
                        c => CaseDecider.Reject(c, body.Reason ?? "", Account(http)),
                        http
                    )
            )
            .RequireAuthorization(Policies.Member);

        cases
            .MapPost(
                "/{id}/retirement",
                (string id, HttpContext http, IEventStore store) =>
                    Execute(store, id, c => CaseDecider.Retire(c, RetireReason.Manual), http)
            )
            .RequireAuthorization(Policies.Admin);

        api.MapPost(
                "/workspaces/{name}/mining",
                async (string name, CaseMining mining, HttpContext http) =>
                    Results.Ok(new { jobs = await mining.EnqueueAsync(name, http.RequestAborted) })
            )
            .WithTags("Cases")
            .RequireAuthorization(Policies.Member);
    }

    private static async Task ApproveAsync(
        IEventStore store,
        NpgsqlDataSource db,
        HttpContext http,
        string id
    )
    {
        await using var connection = await db.OpenConnectionAsync(http.RequestAborted);
        var workspace =
            await connection.QuerySingleOrDefaultAsync<string?>(
                new CommandDefinition(
                    "SELECT workspace FROM casebox.case_catalog WHERE org_id = @Org AND id = @Id",
                    new { Org = http.User.OrgId(), Id = id },
                    cancellationToken: http.RequestAborted
                )
            ) ?? throw new NotFoundException("The case does not exist.");
        var approvedBefore = await connection.ExecuteScalarAsync<int>(
            new CommandDefinition(
                "SELECT count(*) FROM casebox.case_catalog WHERE org_id = @Org AND workspace = @Workspace AND split IS NOT NULL",
                new { Org = http.User.OrgId(), Workspace = workspace },
                cancellationToken: http.RequestAborted
            )
        );
        await store.Execute<Case>(
            Case.StreamId(id),
            c => CaseDecider.Approve(c, Account(http), Case.SplitFor(approvedBefore)),
            http.RequestAborted
        );
    }

    private static async Task<IResult> Execute(
        IEventStore store,
        string id,
        Func<Case, IEnumerable<object>> decide,
        HttpContext http
    )
    {
        await store.Execute<Case>(Case.StreamId(id), decide, http.RequestAborted);
        return Results.NoContent();
    }

    private static string Account(HttpContext http) =>
        http.User.AccountId() ?? throw new DomainException("Only an account reviews cases.");

    private const string Columns = """
        id, kind, workspace, status, scope, source, work_item, rank, fail_to_pass, pass_to_pass, drift, weight, split, instruction,
        mined_at, updated_at, repos::text AS repos, signatures::text AS signatures, assertions::text AS assertions, judge::text AS judge,
        assertions_approved, oracle, seconds, failure_reason, failure_detail, reject_reason, retired_reason, recipe_hash, harness_hash
        """;

    private sealed record Row(
        string Id,
        string Kind,
        string Workspace,
        string Status,
        string Scope,
        string Source,
        string? WorkItem,
        int Rank,
        int? FailToPass,
        int? PassToPass,
        bool Drift,
        double Weight,
        string? Split,
        string? Instruction,
        DateTime MinedAt,
        DateTime UpdatedAt,
        string Repos,
        string Signatures,
        string Assertions,
        string Judge,
        bool AssertionsApproved,
        string? Oracle,
        double? Seconds,
        string? FailureReason,
        string? FailureDetail,
        string? RejectReason,
        string? RetiredReason,
        string RecipeHash,
        string HarnessHash
    )
    {
        public CaseSummary Summary() =>
            new(
                Id,
                Kind,
                Workspace,
                Status,
                Scope,
                Source,
                WorkItem,
                Rank,
                FailToPass,
                PassToPass,
                Drift,
                Weight,
                Split,
                Instruction is not null,
                Utc(MinedAt),
                Utc(UpdatedAt)
            );

        public CaseView View() =>
            new(
                Summary(),
                Parse(Repos),
                Masking.Mask(Instruction),
                Parse(Signatures),
                Parse(Assertions),
                Parse(Judge),
                AssertionsApproved,
                Oracle,
                Seconds,
                FailureReason,
                Masking.Mask(FailureDetail),
                Masking.Mask(RejectReason),
                RetiredReason,
                RecipeHash,
                HarnessHash,
                ApprovalBlocker()
            );

        // Why a person cannot approve the case yet, the rules of CaseDecider.Approve, or null.
        private string? ApprovalBlocker() =>
            Status switch
            {
                "approved" => "The case is approved.",
                "rejected" => "A rejected case is not approved.",
                "retired" => "The case is retired.",
                "mined" => "The case is not validated yet.",
                "validation_failed" => "The case failed validation.",
                _ when Instruction is null => "The case needs an instruction first.",
                _ when Kind == "steering" && !AssertionsApproved =>
                    "A steering case needs approved assertions first.",
                _ => null,
            };

        private static JsonElement Parse(string json) =>
            JsonDocument.Parse(json).RootElement.Clone();

        private static DateTimeOffset Utc(DateTime at) =>
            new(DateTime.SpecifyKind(at, DateTimeKind.Utc));
    }
}
