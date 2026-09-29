using Casebox.Server.Features.Orgs;
using Dapper;
using Deedbox;
using Npgsql;

namespace Casebox.Server.Features.Auth;

public sealed record Account(
    string Id,
    string DisplayName,
    Role Role,
    DateTimeOffset CreatedAt,
    DateTimeOffset LastLoginAt
);

// Account data of the people who log in to the UI. Kept apart from captured work data.
public sealed class AccountStore(NpgsqlDataSource dataSource, TimeProvider clock)
{
    public async Task<Account> LoginAsync(
        string orgId,
        string issuer,
        string subject,
        string displayName,
        Role firstRole,
        CancellationToken ct
    )
    {
        var now = clock.GetUtcNow();
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleAsync<Row>(
            new CommandDefinition(
                """
                INSERT INTO casebox.accounts (id, org_id, issuer, subject, display_name, role, created_at, last_login_at)
                VALUES (@Id, @Org, @Issuer, @Subject, @DisplayName, @Role, @Now, @Now)
                ON CONFLICT (org_id, issuer, subject)
                DO UPDATE SET display_name = EXCLUDED.display_name, last_login_at = EXCLUDED.last_login_at
                RETURNING id, display_name, role, created_at, last_login_at
                """,
                new
                {
                    Id = Ids.New(),
                    Org = orgId,
                    Issuer = issuer,
                    Subject = subject,
                    DisplayName = displayName,
                    Role = RoleName(firstRole),
                    Now = now,
                },
                cancellationToken: ct
            )
        );
        return row.ToAccount();
    }

    public async Task<Role?> RoleOfAsync(string orgId, string accountId, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var role = await connection.QuerySingleOrDefaultAsync<string>(
            new CommandDefinition(
                "SELECT role FROM casebox.accounts WHERE org_id = @Org AND id = @Id",
                new { Org = orgId, Id = accountId },
                cancellationToken: ct
            )
        );
        return role is null ? null : ParseRole(role);
    }

    public async Task<Account?> GetAsync(string orgId, string accountId, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var row = await connection.QuerySingleOrDefaultAsync<Row>(
            new CommandDefinition(
                "SELECT id, display_name, role, created_at, last_login_at FROM casebox.accounts WHERE org_id = @Org AND id = @Id",
                new { Org = orgId, Id = accountId },
                cancellationToken: ct
            )
        );
        return row?.ToAccount();
    }

    public async Task<IReadOnlyList<Account>> ListAsync(string orgId, CancellationToken ct)
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        var rows = await connection.QueryAsync<Row>(
            new CommandDefinition(
                "SELECT id, display_name, role, created_at, last_login_at FROM casebox.accounts WHERE org_id = @Org ORDER BY created_at",
                new { Org = orgId },
                cancellationToken: ct
            )
        );
        return rows.Select(r => r.ToAccount()).ToList();
    }

    // Changes a role and audits it as org.member_role_changed in the same transaction.
    // An organisation always keeps at least one Owner.
    public async Task ChangeRoleAsync(
        string orgId,
        string accountId,
        Role role,
        Role actorRole,
        IEventStore store,
        CancellationToken ct
    )
    {
        await using var connection = await dataSource.OpenConnectionAsync(ct);
        await using var transaction = await connection.BeginTransactionAsync(ct);

        var owners = (
            await connection.QueryAsync<string>(
                new CommandDefinition(
                    "SELECT id FROM casebox.accounts WHERE org_id = @Org AND role = 'owner' FOR UPDATE",
                    new { Org = orgId },
                    transaction,
                    cancellationToken: ct
                )
            )
        ).ToList();
        var current =
            await connection.QuerySingleOrDefaultAsync<string>(
                new CommandDefinition(
                    "SELECT role FROM casebox.accounts WHERE org_id = @Org AND id = @Id FOR UPDATE",
                    new { Org = orgId, Id = accountId },
                    transaction,
                    cancellationToken: ct
                )
            ) ?? throw new NotFoundException("The account does not exist.");

        var currentRole = ParseRole(current);
        if (currentRole == role)
            return;
        if ((role == Role.Owner || currentRole == Role.Owner) && actorRole != Role.Owner)
            throw new DomainException("Only an Owner can grant or remove the Owner role.");
        if (currentRole == Role.Owner && owners.Count == 1)
            throw new ConflictException("An organisation needs at least one Owner.");

        await connection.ExecuteAsync(
            new CommandDefinition(
                "UPDATE casebox.accounts SET role = @Role WHERE org_id = @Org AND id = @Id",
                new
                {
                    Org = orgId,
                    Id = accountId,
                    Role = RoleName(role),
                },
                transaction,
                cancellationToken: ct
            )
        );
        await store
            .UseTransaction(transaction)
            .Append(
                Organisation.StreamId,
                ExpectedVersion.Any,
                [new OrgEvents.MemberRoleChanged(accountId, role)]
            );
        await transaction.CommitAsync(ct);
    }

    public static string RoleName(Role role) => role.ToString().ToLowerInvariant();

    public static Role ParseRole(string role) => Enum.Parse<Role>(role, ignoreCase: true);

    private sealed record Row(
        string Id,
        string DisplayName,
        string Role,
        DateTime CreatedAt,
        DateTime LastLoginAt
    )
    {
        public Account ToAccount() =>
            new(
                Id,
                DisplayName,
                ParseRole(Role),
                new DateTimeOffset(DateTime.SpecifyKind(CreatedAt, DateTimeKind.Utc)),
                new DateTimeOffset(DateTime.SpecifyKind(LastLoginAt, DateTimeKind.Utc))
            );
    }
}
