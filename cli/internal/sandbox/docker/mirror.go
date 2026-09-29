package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/egressproxy"
)

// MirrorURL is where a sandbox reaches the registry mirror: the egress proxy, by its name on the
// sandbox's network.
const MirrorURL = "http://" + ProxyHost + ":" + egressproxy.Port

// MirrorEnv points the package tools at the mirror. It goes into every command's environment.
// GOFLAGS is left alone: the mirror changes where modules come from, not how go.mod is resolved.
// GONOSUMDB, GONOPROXY and GOPRIVATE are emptied, which Go reads as unset, so no module skips the
// mirror or the checksum database.
func MirrorEnv() map[string]string {
	npm := MirrorURL + "/npm/"
	pypi := MirrorURL + "/pypi/simple/"
	return map[string]string{
		"GOPROXY":   MirrorURL + "/go/",
		"GOSUMDB":   "sum.golang.org",
		"GONOSUMDB": "",
		"GONOPROXY": "",
		"GOPRIVATE": "",

		"npm_config_registry":        npm,
		"BUN_CONFIG_REGISTRY":        npm,
		"YARN_NPM_REGISTRY_SERVER":   npm,
		"YARN_UNSAFE_HTTP_WHITELIST": ProxyHost,
		"YARN_REGISTRY":              npm,

		"PIP_INDEX_URL":    pypi,
		"PIP_TRUSTED_HOST": ProxyHost,
		"UV_DEFAULT_INDEX": pypi,
		"UV_INDEX_URL":     pypi,
		"UV_INSECURE_HOST": ProxyHost,
	}
}

// MirrorFiles are the tool configuration files the mirror needs, by path below the sandbox user's
// home: NuGet's user config, Maven's user settings and a Gradle init script.
func MirrorFiles() map[string]string {
	return map[string]string{
		".nuget/NuGet/NuGet.Config":            nugetConfig,
		".m2/settings.xml":                     mavenSettings,
		".gradle/init.d/casebox-mirror.gradle": gradleInit,
	}
}

// NuGet merges the configs from the repository up to the user's; a repository's own nuget.org
// source keeps its usual key, so it is disabled here, and HTTP needs allowInsecureConnections.
const nugetConfig = `<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="casebox-mirror" value="` + MirrorURL + `/nuget/v3/index.json" protocolVersion="3" allowInsecureConnections="true" />
  </packageSources>
  <disabledPackageSources>
    <add key="nuget.org" value="true" />
  </disabledPackageSources>
</configuration>
`

const mavenSettings = `<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0"
          xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
          xsi:schemaLocation="http://maven.apache.org/SETTINGS/1.0.0 https://maven.apache.org/xsd/settings-1.0.0.xsd">
  <mirrors>
    <mirror>
      <id>casebox-mirror</id>
      <name>Casebox registry mirror</name>
      <mirrorOf>central</mirrorOf>
      <url>` + MirrorURL + `/maven/</url>
      <blocked>false</blocked>
    </mirror>
  </mirrors>
</settings>
`

// The init script sends every Maven Central repository (mavenCentral() and any repository with
// its URL) through the mirror: the settings' plugin and dependency repositories and every
// project's.
const gradleInit = `// Casebox: Maven Central goes through the registry mirror of this sandbox.
def caseboxMirror = new URI('` + MirrorURL + `/maven/')
def caseboxCentral = ['repo.maven.apache.org', 'repo1.maven.org'] as Set
def caseboxRedirect = { repositories ->
    repositories.all { repo ->
        if (repo instanceof MavenArtifactRepository && repo.url != null && caseboxCentral.contains(repo.url.host)) {
            repo.url = caseboxMirror
            if (repo.hasProperty('allowInsecureProtocol')) {
                repo.allowInsecureProtocol = true
            }
        }
    }
}
beforeSettings { settings ->
    caseboxRedirect(settings.pluginManagement.repositories)
    if (settings.hasProperty('dependencyResolutionManagement')) {
        caseboxRedirect(settings.dependencyResolutionManagement.repositories)
    }
}
allprojects { project ->
    caseboxRedirect(project.buildscript.repositories)
    caseboxRedirect(project.repositories)
}
`

// mirrorArgs are the proxy's flags for m.
func mirrorArgs(m sandbox.Mirror) []string {
	return []string{
		"-mirror", MirrorURL,
		"-deny-go", strings.Join(m.Denied.Go, ","),
		"-deny-npm", strings.Join(m.Denied.NPM, ","),
		"-deny-python", strings.Join(m.Denied.Python, ","),
		"-deny-nuget", strings.Join(m.Denied.NuGet, ","),
		"-deny-maven", strings.Join(m.Denied.Maven, ","),
	}
}

func egressDeny(m sandbox.Mirror) egressproxy.Deny {
	return egressproxy.Deny{Go: m.Denied.Go, NPM: m.Denied.NPM, Python: m.Denied.Python, NuGet: m.Denied.NuGet, Maven: m.Denied.Maven}
}

// writeMirrorFiles writes MirrorFiles into the sandbox user's home, as that user: the container
// runs with no capabilities, so root could not write into a home it does not own.
func (p *Provider) writeMirrorFiles(ctx context.Context, sb sandbox.Sandbox) error {
	files := MirrorFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	script := `set -e; test -n "$HOME" && test "$HOME" != /; f="$HOME/$1"; mkdir -p "${f%/*}"; cat >"$f"`
	for _, name := range names {
		res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", script, "sh", name}, Stdin: strings.NewReader(files[name])})
		if err != nil {
			return fmt.Errorf("write the registry mirror's %s: %w", name, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("write the registry mirror's %s: exit %d: %s", name, res.ExitCode, lastLines(string(res.Stderr), 10))
		}
	}
	return nil
}
