package views

import "testing"

func TestAuthFailure(t *testing.T) {
	yes := []string{
		"Failed to authenticate: OAuth session expired and could not be refreshed",
		"Failed to authenticate. API Error: 401 OAuth access token has been revoked.",
		"codex: not logged in",
	}
	for _, d := range yes {
		if !authFailure(d) {
			t.Errorf("authFailure(%q) = false", d)
		}
	}
	for _, d := range []string{"", "API error: overloaded", "context deadline exceeded"} {
		if authFailure(d) {
			t.Errorf("authFailure(%q) = true", d)
		}
	}
}

func TestProvidersTitle(t *testing.T) {
	cases := map[string]string{
		providersTitle(0, 0): "Providers",
		providersTitle(2, 0): "Providers: 2 with an update available",
		providersTitle(0, 1): "Providers: 1 not signed in",
		providersTitle(1, 1): "Providers: 1 not signed in, 1 with an update available",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
