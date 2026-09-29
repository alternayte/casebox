using System.Globalization;
using System.Text.RegularExpressions;

namespace Casebox.Server.Features.Privacy;

// Pseudonym period IDs: "2026-Q3", "2026-09" or "2026" (Features/Capture/Identities.PeriodOf).
public static partial class Periods
{
    [GeneratedRegex(@"^(?<y>\d{4})(-Q(?<q>[1-4])|-(?<m>\d{2}))?$")]
    private static partial Regex Pattern();

    // The first moment after the period, or null for an ID Casebox did not make.
    public static DateTimeOffset? EndOf(string period)
    {
        var m = Pattern().Match(period);
        if (!m.Success)
            return null;
        var year = int.Parse(m.Groups["y"].Value, CultureInfo.InvariantCulture);
        if (m.Groups["q"].Success)
            return new DateTimeOffset(year, 1, 1, 0, 0, 0, TimeSpan.Zero).AddMonths(
                3 * int.Parse(m.Groups["q"].Value, CultureInfo.InvariantCulture)
            );
        if (m.Groups["m"].Success)
            return new DateTimeOffset(
                year,
                int.Parse(m.Groups["m"].Value, CultureInfo.InvariantCulture),
                1,
                0,
                0,
                0,
                TimeSpan.Zero
            ).AddMonths(1);
        return new DateTimeOffset(year + 1, 1, 1, 0, 0, 0, TimeSpan.Zero);
    }
}
