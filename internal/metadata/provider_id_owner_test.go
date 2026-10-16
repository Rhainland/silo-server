package metadata

import "testing"

func TestIdentityProviderIDsContradict(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"different TMDB, shared TVDB (#2263)",
			map[string]string{"tmdb": "125249", "imdb": "tt0035665", "tvdb": "66484"},
			map[string]string{"tmdb": "43506", "imdb": "tt0035664", "tvdb": "66484"}, true},
		{"different IMDb, shared TVDB, one TMDB",
			map[string]string{"imdb": "tt0035665", "tvdb": "66484"},
			map[string]string{"tmdb": "43506", "imdb": "tt0035664", "tvdb": "66484"}, true},
		{"same IMDb, different TVDB",
			map[string]string{"imdb": "tt1", "tvdb": "7"},
			map[string]string{"imdb": "tt1", "tvdb": "8"}, false},
		{"same TMDB, remapped TVDB",
			map[string]string{"tmdb": "949", "tvdb": "2002"},
			map[string]string{"tmdb": "949", "tvdb": "1001"}, false},
		{"only TVDB shared and different",
			map[string]string{"tvdb": "1", "tmdb": "5"},
			map[string]string{"tvdb": "2", "imdb": "tt1"}, true},
		{"only IMDb shared and equal",
			map[string]string{"tmdb": "1", "imdb": "tt1"},
			map[string]string{"tvdb": "9", "imdb": "tt1"}, false},
		{"TMDB in its slug URL form",
			map[string]string{"tmdb": "1931", "imdb": "tt1"},
			map[string]string{"tmdb": "1931-disney-s-adventures-of-the-gummi-bears", "imdb": "tt2"}, false},
		{"different TMDB, one in slug form",
			map[string]string{"tmdb": "19310"},
			map[string]string{"tmdb": "1931-disney-s-adventures-of-the-gummi-bears"}, true},
		{"nothing shared", map[string]string{"tmdb": "1"}, map[string]string{"tvdb": "2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityProviderIDsContradict(tc.a, tc.b); got != tc.want {
				t.Fatalf("identityProviderIDsContradict = %v, want %v", got, tc.want)
			}
		})
	}
}
