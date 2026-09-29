using System.Text.Json;
using Casebox.Server.Infrastructure;

namespace Casebox.Server.Tests.Infrastructure;

// The API's JSON: web defaults with snake_case enum names.
public static class Json
{
    public static readonly JsonSerializerOptions Options = new(JsonSerializerDefaults.Web)
    {
        Converters = { CaseboxStreams.Enums },
    };
}
