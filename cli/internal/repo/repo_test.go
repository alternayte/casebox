package repo

import "testing"

func TestRemotesNormalizeToHostOwnerName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Acme/Payments-API.git":             "github.com/acme/payments-api",
		"https://github.com/acme/payments-api":             "github.com/acme/payments-api",
		"https://token@github.com/acme/payments-api.git":   "github.com/acme/payments-api",
		"ssh://git@github.example.com:2222/acme/tools.git": "github.example.com/acme/tools",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
