package runner_test

import (
	"reflect"
	"testing"

	"github.com/alternayte/casebox/cli/internal/runner"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

func TestDeniedPackages(t *testing.T) {
	files := map[string][]byte{
		"go.mod":                              []byte("// the service\nmodule github.com/Acme/widget // main\n\ngo 1.26\n"),
		"tools/go.mod":                        []byte("module \"github.com/acme/widget/tools\"\n"),
		"testdata/fixture/go.mod":             []byte("module example.com/fixture\n"),
		"vendor/x/go.mod":                     []byte("module example.com/vendored\n"),
		"web/package.json":                    []byte(`{"name": "@acme/web", "dependencies": {"react": "18"}}`),
		"package.json":                        []byte(`{"private": true}`),
		"web/node_modules/react/package.json": []byte(`{"name": "react"}`),
		"py/pyproject.toml":                   []byte("[build-system]\nname = \"not-this\"\n\n[project]\nname = \"acme-widget\" # the lib\nversion = \"1\"\n\n[project.urls]\nname = 'nor-this'\n"),
		"old/pyproject.toml":                  []byte("[tool.poetry]\nname = 'acme_legacy'\n"),
		"src/Acme.Widget/Acme.Widget.csproj":  []byte(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><PackageId>Acme.Widget.Core</PackageId><AssemblyName>Acme.Widget.Asm</AssemblyName></PropertyGroup></Project>`),
		"src/Tool/Tool.fsproj":                []byte(`<Project><PropertyGroup><PackageId>$(AssemblyName).Pkg</PackageId></PropertyGroup></Project>`),
		"pom.xml":                             []byte(`<project><modelVersion>4.0.0</modelVersion><groupId>com.acme</groupId><artifactId>parent</artifactId><dependencies><dependency><groupId>junit</groupId><artifactId>junit</artifactId></dependency></dependencies></project>`),
		"core/pom.xml":                        []byte(`<project><parent><groupId>com.acme</groupId><artifactId>parent</artifactId></parent><artifactId>core</artifactId></project>`),
		"README.md":                           []byte("module not.this\n"),
	}
	got := runner.DeniedPackages(files)
	want := sandbox.Mirror{Denied: sandbox.Denied{
		Go:     []string{"github.com/Acme/widget", "github.com/acme/widget/tools"},
		NPM:    []string{"@acme/web"},
		Python: []string{"acme-widget", "acme_legacy"},
		NuGet:  []string{"Acme.Widget", "Acme.Widget.Asm", "Acme.Widget.Core", "Tool"},
		Maven:  []string{"com.acme:core", "com.acme:parent"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeniedPackages =\n%+v\nwant\n%+v", got, want)
	}
}
