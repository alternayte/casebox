package repo

// Cases is the cases block of casebox.yml: it narrows mining (docs/specs/cases.md, "Filters" and
// "Test files"). Every field is optional.
type Cases struct {
	Window         int      `yaml:"window,omitempty"`           // days back from now; default 183
	MaxSourceFiles int      `yaml:"max_source_files,omitempty"` // default 12
	TestGlobs      []string `yaml:"test_globs,omitempty"`       // replaces DefaultTestGlobs
}

// Mining defaults of docs/specs/cases.md.
const (
	DefaultCaseWindowDays = 183
	DefaultMaxSourceFiles = 12
)

// DefaultTestGlobs are the paths of test files, for a casebox.yml that names none.
var DefaultTestGlobs = []string{
	"**/*_test.go",
	"**/*Tests.cs", "**/*Test.cs", "**/*.Tests/**", "**/*.UnitTests/**", "**/*.IntegrationTests/**",
	"**/*.test.js", "**/*.test.jsx", "**/*.test.ts", "**/*.test.tsx", "**/*.test.mjs", "**/*.test.cjs",
	"**/*.spec.*", "**/__tests__/**",
	"**/test_*.py", "**/*_test.py", "**/tests/**", "**/test/**",
	"**/src/test/**",
}

// TestGlobs returns the test globs of casebox.yml, or the default list.
func (c Config) TestGlobs() []string {
	if len(c.Cases.TestGlobs) > 0 {
		return c.Cases.TestGlobs
	}
	return DefaultTestGlobs
}

// IsTestFile reports whether a repo-relative path is a test file under globs.
func IsTestFile(globs []string, file string) bool { return matchAny(globs, file) }
