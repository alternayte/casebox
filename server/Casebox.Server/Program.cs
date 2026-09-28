var builder = WebApplication.CreateBuilder(args);

var app = builder.Build();

// The web UI is built into wwwroot by `just web-build`; every unknown path falls back to the SPA.
app.UseDefaultFiles();
app.UseStaticFiles();
app.MapFallbackToFile("index.html");

app.Run();

public partial class Program;
