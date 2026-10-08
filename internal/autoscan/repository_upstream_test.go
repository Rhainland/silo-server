package autoscan

import "testing"

// Saving the same server's URL spelled differently must not reset the bound
// sources' markers: the next poll would start from now and skip whatever the
// server imported since the last one.
func TestConnectionUpstreamIgnoresTheSameURLSpelledDifferently(t *testing.T) {
	stored := "http://Sonarr.example:8989"
	for _, tc := range []struct {
		next    string
		differs bool
	}{
		{"http://sonarr.example:8989", false},
		{"http://sonarr.example:8989/", false},
		{" HTTP://SONARR.EXAMPLE:8989/ ", false},
		{"http://sonarr.example:8990", true},
		{"https://sonarr.example:8989", true},
		{"http://sonarr.example:8989/sonarr", true},
		{"http://radarr.example:7878", true},
	} {
		if got := (connectionUpstream{baseURL: &stored}).differsFrom(Connection{BaseURL: tc.next}); got != tc.differs {
			t.Errorf("%q -> %q: differs = %v, want %v", stored, tc.next, got, tc.differs)
		}
	}
}
