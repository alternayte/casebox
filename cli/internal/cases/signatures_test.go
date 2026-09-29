package cases

import (
	"fmt"
	"strings"
	"testing"
)

// patch writes a one-hunk diff that adds lines to a file; a line starting with "~" is context.
func patch(file string, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nindex 1111111..2222222 100644\n--- a/%s\n+++ b/%s\n@@ -1,1 +1,%d @@\n", file, file, file, file, len(lines))
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "~"); ok {
			b.WriteString(" " + rest + "\n")
			continue
		}
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func TestSignatures(t *testing.T) {
	for _, c := range []struct {
		name   string
		source string
		tests  string
		want   []string
	}{
		{
			name: "go funcs, methods and types the tests use",
			source: patch("store/store.go",
				"~package store",
				"",
				"// Store keeps items.",
				"type Store struct {",
				"\titems map[string]Item",
				"}",
				"",
				"func (s *Store) Get(id string) (Item, error) {",
				"\treturn s.items[id], nil",
				"}",
				"",
				"func NewStore() *Store { return &Store{} }",
				"",
				"func helper() {}",
				"",
				"func Unused() {}",
			),
			tests: patch("store/store_test.go", "func TestGet(t *testing.T) {", "\tvar s *Store = NewStore()", "\t_, _ = s.Get(\"a\")", "}"),
			want:  []string{"type Store struct {\n\titems map[string]Item\n}", "func (s *Store) Get(id string) (Item, error)", "func NewStore() *Store"},
		},
		{
			name:   "a go signature changed without its body",
			source: patch("store/store.go", "func Get(id string, strict bool) (Item, error) {", "~\treturn nil"),
			tests:  patch("store/store_test.go", "\tGet(\"a\", true)"),
			want:   []string{"func Get(id string, strict bool) (Item, error)"},
		},
		{
			name: "go grouped types",
			source: patch("kinds.go",
				"type (",
				"\tKind string",
				"\tlabel string",
				")",
			),
			tests: patch("kinds_test.go", "var _ Kind = \"x\""),
			want:  []string{"type Kind string"},
		},
		{
			name: "c# public members and types",
			source: patch("src/Api/Store.cs",
				"public sealed class Store",
				"{",
				"    public Store(IClock clock) { }",
				"    public async Task<Item?> GetAsync(string id,",
				"        CancellationToken ct) => await Find(id, ct);",
				"    public int Count { get; }",
				"    private void Hidden() { }",
				"}",
				"public interface IStore",
				"{",
				"    Task<Item?> GetAsync(string id, CancellationToken ct);",
				"}",
				"public record Item(string Id, int Qty);",
			),
			tests: patch("tests/Api.Tests/StoreTests.cs", "var store = new Store(clock);", "IStore s = store;", "var item = await store.GetAsync(\"a\", ct);", "Assert.Equal(2, store.Count);", "Assert.Equal(new Item(\"a\", 1), item);"),
			want: []string{
				"public sealed class Store",
				"public Store(IClock clock)",
				"public async Task<Item?> GetAsync(string id,\n        CancellationToken ct)",
				"public int Count",
				"public interface IStore\n{\n    Task<Item?> GetAsync(string id, CancellationToken ct);\n}",
				"public record Item(string Id, int Qty)",
			},
		},
		{
			name: "typescript exported functions, classes and types",
			source: patch("web/src/cart.ts",
				"export interface Cart {",
				"  items: Item[];",
				"}",
				"export type Total = { amount: number };",
				"export async function addItem(cart: Cart, item: Item): Promise<Cart> {",
				"  return cart;",
				"}",
				"export const total = (cart: Cart): Total => ({ amount: 0 });",
				"export const LIMIT = 5;",
				"function internal() {}",
				"export class CartStore {",
				"}",
			),
			tests: patch("web/src/cart.test.ts", "const c: Cart = { items: [] };", "await addItem(c, item);", "expect(total(c)).toEqual<Total>({ amount: 0 });", "new CartStore();", "expect(LIMIT).toBe(5);"),
			want: []string{
				"export interface Cart {\n  items: Item[];\n}",
				"export type Total = { amount: number };",
				"export async function addItem(cart: Cart, item: Item): Promise<Cart>",
				"export const total = (cart: Cart): Total",
				"export class CartStore",
			},
		},
		{
			name: "python top-level def and class",
			source: patch("pkg/store.py",
				"class Store:",
				"    def get(self, key):",
				"        return None",
				"",
				"def load(path: str,",
				"         strict: bool = False) -> Store:",
				"    return Store()",
				"",
				"async def fetch(url) -> bytes:",
				"    pass",
			),
			tests: patch("tests/test_store.py", "def test_load():", "    s = load('a')", "    assert isinstance(s, Store)", "    fetch('x')"),
			want:  []string{"class Store:", "def load(path: str,\n         strict: bool = False) -> Store:", "async def fetch(url) -> bytes:"},
		},
		{
			name: "java public members and types",
			source: patch("src/main/java/com/acme/Store.java",
				"public final class Store {",
				"    public Store(Clock clock) {}",
				"    public <T> Optional<T> find(String id, Class<T> type) {",
				"        return Optional.empty();",
				"    }",
				"    private void hidden() {}",
				"}",
				"public enum Kind {",
				"    A, B",
				"}",
			),
			tests: patch("src/test/java/com/acme/StoreTest.java", "Store s = new Store(clock);", "s.find(\"a\", Item.class);", "assertEquals(Kind.A, k);"),
			want: []string{
				"public final class Store",
				"public Store(Clock clock)",
				"public <T> Optional<T> find(String id, Class<T> type)",
				"public enum Kind {\n    A, B\n}",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Signatures([]byte(c.source), []byte(c.tests))
			if strings.Join(got, "\n---\n") != strings.Join(c.want, "\n---\n") {
				t.Errorf("signatures:\n%s\nwant:\n%s", strings.Join(got, "\n---\n"), strings.Join(c.want, "\n---\n"))
			}
		})
	}
}

func TestValidAssertions(t *testing.T) {
	got := ValidAssertions([]Assertion{
		{Kind: "forbidden_file", Path: "migrations/**"},
		{Kind: "forbidden_file"},
		{Kind: "command_before_done", Pattern: "go test"},
		{Kind: "diff_must_not_match", Pattern: "gomock("},
		{Kind: "diff_must_match", Pattern: `(?m)^\+.*context\.Context`, Path: "ignored"},
		{Kind: "run_linter", Pattern: "x"},
	})
	want := []Assertion{
		{Kind: "forbidden_file", Path: "migrations/**"},
		{Kind: "command_before_done", Pattern: "go test"},
		{Kind: "diff_must_match", Pattern: `(?m)^\+.*context\.Context`},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("assertions %+v, want %+v", got, want)
	}
}
