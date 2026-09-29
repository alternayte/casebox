using System.Text.Json;
using Casebox.Server.Features.Capture;
using Casebox.Server.Tests.Infrastructure;

namespace Casebox.Server.Tests.Features;

// The server's second redaction pass runs the same corpus as the CLI (testdata/redaction-corpus.json).
public sealed class RedactionTests
{
    public static TheoryData<string> Cases()
    {
        var data = new TheoryData<string>();
        foreach (var c in Corpus().RootElement.GetProperty("cases").EnumerateArray())
            data.Add(c.GetProperty("name").GetString()!);
        return data;
    }

    [Theory]
    [MemberData(nameof(Cases))]
    public void The_corpus_case_is_redacted(string name)
    {
        var c = Corpus().RootElement.GetProperty("cases").EnumerateArray().Single(x => x.GetProperty("name").GetString() == name);
        var output = Redaction.Redact(c.GetProperty("input").GetString()!);
        foreach (var absent in c.GetProperty("absent").EnumerateArray())
            Assert.DoesNotContain(absent.GetString()!, output, StringComparison.Ordinal);
        foreach (var present in c.GetProperty("present").EnumerateArray())
            Assert.Contains(present.GetString()!, output, StringComparison.Ordinal);
    }

    private static JsonDocument Corpus() =>
        JsonDocument.Parse(File.ReadAllText(Path.Combine(StackFixture.RepoRoot(), "testdata", "redaction-corpus.json")));
}
