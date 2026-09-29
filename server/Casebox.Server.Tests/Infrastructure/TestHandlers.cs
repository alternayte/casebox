using System.Collections.Concurrent;
using System.Data.Common;
using Casebox.Server.Features.Inbox;
using Casebox.Server.Features.Jobs;
using Casebox.Server.Features.Workspaces;
using Deedbox;
using QueueBox.Inbox;

namespace Casebox.Server.Tests.Infrastructure;

// test.create_workspace: creates the named workspace in the inbox transaction, then fails when
// the payload asks it to, so a test can see the events roll back with the inbox row.
public sealed class TestInboxHandler : IInboxHandler
{
    public static readonly ConcurrentDictionary<string, int> Attempts = new();

    public string Source => InboxSources.Poll;

    public string EventType => "test.create_workspace";

    public async Task HandleAsync(
        InboxMessage message,
        IEventStore store,
        DbTransaction transaction,
        CancellationToken ct
    )
    {
        var payload = message.Payload.GetProperty("payload");
        var name = payload.GetProperty("workspace").GetString()!;
        Attempts.AddOrUpdate(name, 1, (_, n) => n + 1);
        await store.Execute<Workspace>(
            Workspace.StreamIdFor(name),
            w => w.Exists ? [] : WorkspaceDecider.Create(w, name)
        );
        if (payload.TryGetProperty("fail", out var fail) && fail.GetBoolean())
            throw new InvalidOperationException("The test handler fails on purpose.");
    }
}

// test.job: a worker's result adds a repo to a workspace, with an expected version.
public sealed class TestJobHandler : IJobResultHandler
{
    public const string JobKind = "test.job";

    public string Kind => JobKind;

    public async Task HandleAsync(JobResult result, CancellationToken ct)
    {
        var workspace = result.Job.Payload.GetProperty("workspace").GetString()!;
        var repo = result.Result.GetProperty("repo").GetString()!;
        var (state, version) = await result.Store.Load<Workspace>(Workspace.StreamIdFor(workspace));
        var events = WorkspaceDecider.AddRepo(state, repo).ToList();
        if (events.Count > 0)
            await result.Store.Append(
                Workspace.StreamIdFor(workspace),
                ExpectedVersion.Exact(version),
                events
            );
    }
}

// test.idle: a kind that no test leases, so a seeded job keeps its state.
public sealed class IdleJobHandler : IJobResultHandler
{
    public const string JobKind = "test.idle";

    public string Kind => JobKind;

    public Task HandleAsync(JobResult result, CancellationToken ct) => Task.CompletedTask;
}

// The roster a GitHub organisation or Jira project would give: two identities of one person, and
// the team the steering tests need to reach k.
public sealed class TestRoster : Casebox.Server.Features.Privacy.IRosterSource
{
    public const string Canonical = "github:ada-roster";
    public const string Email = "email:ada.roster@example.com";

    // Steering tests' people: each a GitHub login with a git email.
    public static readonly string[] Team = ["grace", "linus", "margaret", "edsger"];

    public static string TeamEmail(string login) => $"{login}.team@example.com";

    public Task<IReadOnlyList<Casebox.Server.Features.Privacy.RosterPerson>> PeopleAsync(
        string orgId,
        CancellationToken ct
    ) =>
        Task.FromResult<IReadOnlyList<Casebox.Server.Features.Privacy.RosterPerson>>([
            new(Canonical, [Email, "jira:ada"]),
            .. Team.Select(t => new Casebox.Server.Features.Privacy.RosterPerson(
                $"github:{t}-team",
                [$"email:{TeamEmail(t)}"]
            )),
        ]);
}
