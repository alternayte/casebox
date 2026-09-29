package stack

import "testing"

// The image tag must be one the images workflow publishes: a release version, or edge.
func TestTheServerImageMatchesTheCLIVersion(t *testing.T) {
	for version, want := range map[string]string{
		"0.1.0-dev":      "edge",
		"":               "edge",
		"0.1.0-trial.1":  "0.1.0-trial.1",
		"v0.1.0-trial.1": "0.1.0-trial.1",
		"0.1.0":          "0.1.0",
	} {
		if got := ImageTag(version); got != want {
			t.Errorf("ImageTag(%q) = %q, want %q", version, got, want)
		}
	}
}
