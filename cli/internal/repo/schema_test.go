package repo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const schemaPath = "../../../docs/public/schema/casebox.schema.json"

// Every casebox.yml example cannot go stale: the ones in the docs (fenced as
// ```yaml title="casebox.yml"), the examples folder and the file init writes validate against the
// published schema, and the schema names every key the CLI reads.
func TestExamplesMatchTheSchema(t *testing.T) {
	c := jsonschema.NewCompiler()
	schema, err := c.Compile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	examples := map[string]string{}
	fence := regexp.MustCompile("(?s)```yaml title=\"[^\"]*casebox\\.yml\"\\n(.*?)```")
	_ = filepath.WalkDir("../../../docs/src/content/docs", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".mdx")) {
			return err
		}
		body, _ := os.ReadFile(p)
		for i, m := range fence.FindAllStringSubmatch(string(body), -1) {
			examples[p+"#"+string(rune('a'+i))] = m[1]
		}
		return nil
	})
	if matches, _ := filepath.Glob("../../../examples/*casebox.yml"); len(matches) > 0 {
		for _, p := range matches {
			body, _ := os.ReadFile(p)
			examples[p] = string(body)
		}
	}
	if len(examples) < 3 {
		t.Fatalf("found %d casebox.yml examples in the docs; the configuration reference holds at least 3", len(examples))
	}
	for name, text := range examples {
		var v any
		if err := yaml.Unmarshal([]byte(text), &v); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		raw, _ := json.Marshal(v)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(doc); err != nil {
			t.Errorf("%s does not match the schema: %v", name, err)
		}
		var cfg Config
		if err := yaml.Unmarshal([]byte(text), &cfg); err != nil {
			t.Errorf("%s: the CLI cannot read it: %v", name, err)
		}
	}

	var root map[string]any
	raw, _ := os.ReadFile(schemaPath)
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	covers(t, "", reflect.TypeOf(Config{}), root)
}

// covers fails for a yaml key of the type that the schema object does not name.
func covers(t *testing.T, path string, typ reflect.Type, schema map[string]any) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
		if items, ok := schema["items"].(map[string]any); ok {
			schema = items
		}
	}
	if typ.Kind() == reflect.Map {
		if extra, ok := schema["additionalProperties"].(map[string]any); ok {
			covers(t, path+".*", typ.Elem(), extra)
		}
		return
	}
	if typ.Kind() != reflect.Struct || typ.Name() == "Duration" {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		sub, ok := props[key].(map[string]any)
		if !ok {
			t.Errorf("the schema does not name %s.%s", path, key)
			continue
		}
		covers(t, path+"."+key, f.Type, sub)
	}
}
