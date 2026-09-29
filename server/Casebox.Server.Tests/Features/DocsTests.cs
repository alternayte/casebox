using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Casebox.Server.Tests.Infrastructure;

namespace Casebox.Server.Tests.Features;

// The docs site's reference pages that come from the server cannot go stale: the OpenAPI document,
// the API page and the event catalogue are generated here and compared with the committed files.
// Set CASEBOX_UPDATE_DOCS=1 to write them after a deliberate change.
public sealed class DocsTests(StackFixture stack)
{
    private static CancellationToken Ct => TestContext.Current.CancellationToken;

    private static readonly string Docs = Path.Combine(Root(), "docs");

    [Fact]
    public async Task The_API_reference_matches_the_server()
    {
        var document = JsonNode
            .Parse(await stack.ServerA.Anonymous().GetStringAsync("/openapi/v1.json", Ct))!
            .AsObject();
        // The servers list names the test's port; the published document has none.
        document.Remove("servers");
        var json = document.ToJsonString(new JsonSerializerOptions { WriteIndented = true }) + "\n";
        Compare(Path.Combine(Docs, "public", "openapi.json"), json);
        Compare(
            Path.Combine(Docs, "src", "content", "docs", "reference", "api.md"),
            ApiPage(document)
        );
    }

    [Fact]
    public void The_event_catalogue_matches_the_lockfile()
    {
        var lockfile = File.ReadAllLines(
            Path.Combine(Root(), "server", "Casebox.Server.Tests", "Features", "events.lock")
        );
        Compare(
            Path.Combine(Docs, "src", "content", "docs", "reference", "events.md"),
            EventsPage(lockfile)
        );
    }

    // The harness CI tutorial shows the comment the effect writes, from the renderer itself.

    private static string ApiPage(JsonObject document)
    {
        var s = new StringBuilder();
        s.Append(
            """
            ---
            title: API
            description: Every route of the Casebox REST API, generated from the server's OpenAPI document.
            ---

            This page lists every route of the API under `/api/v1`, grouped as the server groups them. The CLI, the GitHub Action and the web UI use only these routes. The full request and answer shapes are in the [OpenAPI document](/openapi.json).

            - A person calls the API with a session cookie and the `X-CSRF-TOKEN` header, or with a CLI token from `casebox init` as `Authorization: Bearer cbx_cli_…`.
            - Workers use `/worker/v1` with a worker token, and capture uses `/ingest/v1` with an ingest token.
            - Every refusal is a problem answer with a `code` and a `type` that links its fix: see [error codes](/reference/errors/).

            """
        );
        var routes = new SortedDictionary<string, List<(string Method, string Path)>>(
            StringComparer.Ordinal
        );
        foreach (var (path, item) in document["paths"]!.AsObject())
        foreach (var (method, operation) in item!.AsObject())
        {
            var tag = operation?["tags"]?[0]?.GetValue<string>() ?? "Other";
            if (!routes.TryGetValue(tag, out var list))
                routes[tag] = list = [];
            list.Add((method.ToUpperInvariant(), path));
        }
        foreach (var (tag, list) in routes)
        {
            s.Append(
                CultureInfo.InvariantCulture,
                $"\n## {tag}\n\n| Method | Path |\n| --- | --- |\n"
            );
            foreach (
                var (method, path) in list.OrderBy(r => r.Path, StringComparer.Ordinal)
                    .ThenBy(r => r.Method, StringComparer.Ordinal)
            )
                s.Append(CultureInfo.InvariantCulture, $"| {method} | `{path}` |\n");
        }
        return s.ToString();
    }

    private static string EventsPage(IEnumerable<string> lockfile)
    {
        var s = new StringBuilder();
        s.Append(
            """
            ---
            title: Event catalogue
            description: Every event Casebox records, its stream and its fields, generated from the event contract lockfile.
            ---

            This page lists every event Casebox records, by stream. Casebox stores each business decision as a Deedbox event, and projections build the pages from them. A field marked `pd(person)` is personal data: erasing that person makes it unreadable in every stream.

            """
        );
        foreach (var raw in lockfile)
        {
            var line = raw.TrimEnd();
            if (line.StartsWith("stream ", StringComparison.Ordinal))
                s.Append(
                    CultureInfo.InvariantCulture,
                    $"\n## {line["stream ".Length..].Split(' ')[0]}\n\n| Event | Fields |\n| --- | --- |\n"
                );
            else if (line.StartsWith("  ", StringComparison.Ordinal))
            {
                var t = line.Trim();
                var space = t.IndexOf(' ', t.IndexOf(' ') + 1);
                var name = t[..space];
                var fields = t[(space + 1)..].Replace("|", "\\|", StringComparison.Ordinal);
                s.Append(CultureInfo.InvariantCulture, $"| `{name}` | `{fields}` |\n");
            }
        }
        return s.ToString();
    }

    private static void Compare(string path, string expected)
    {
        if (Environment.GetEnvironmentVariable("CASEBOX_UPDATE_DOCS") == "1")
        {
            Directory.CreateDirectory(Path.GetDirectoryName(path)!);
            File.WriteAllText(path, expected);
            return;
        }
        Assert.True(
            File.Exists(path),
            $"{path} is missing; run the tests with CASEBOX_UPDATE_DOCS=1"
        );
        Assert.True(
            File.ReadAllText(path) == expected,
            $"{path} is stale; run the tests with CASEBOX_UPDATE_DOCS=1 and commit it"
        );
    }

    private static string Root()
    {
        var dir = new DirectoryInfo(AppContext.BaseDirectory);
        while (dir is not null && !File.Exists(Path.Combine(dir.FullName, "justfile")))
            dir = dir.Parent;
        return dir?.FullName
            ?? throw new InvalidOperationException(
                "The repository root is not above the test binary."
            );
    }
}
