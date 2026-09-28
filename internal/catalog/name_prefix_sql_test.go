package catalog

import (
	"strings"
	"testing"
)

// TestQueryExecutor_NamePrefix_PushedIntoWHERE asserts that when AccessFilter
// carries a NamePrefix, the resulting paged SQL contains a prefix-anchored LIKE
// on the idx_media_items_sort_key expression (migration 102) and nothing else.
// A raw LOWER(title) arm would list title="The Office" (sort_title
// "Office, The") under both T and O. The LIKE pattern argument is anchored with
// no leading wildcard.
func TestQueryExecutor_NamePrefix_PushedIntoWHERE(t *testing.T) {
	exec := &QueryExecutor{Scope: "movie", BaseRelationSQL: "media_items mi"}
	access := AccessFilter{NamePrefix: "Star"}

	sql, args, err := exec.buildPreviewPageSQL(QueryDefinition{}, access, 20, 0, true)
	if err != nil {
		t.Fatalf("buildPreviewPageSQL returned error: %v", err)
	}

	if !strings.Contains(sql, "LOWER(COALESCE(NULLIF(BTRIM(mi.sort_title), ''), mi.title)) LIKE") {
		t.Fatalf("expected sort-key LIKE matching idx_media_items_sort_key; got %q", sql)
	}
	if strings.Contains(sql, "LOWER(mi.title) LIKE") {
		t.Fatalf("prefix must not match the raw title alongside the sort key; got %q", sql)
	}
	if strings.Contains(sql, "LIKE '%") {
		t.Fatalf("expected LIKE pattern to be parameterized (no leading literal wildcard); got %q", sql)
	}

	foundArg := false
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if strings.HasPrefix(strings.ToLower(s), "star") &&
			strings.HasSuffix(s, "%") &&
			!strings.HasPrefix(s, "%") {
			foundArg = true
			break
		}
	}
	if !foundArg {
		t.Fatalf("expected an args entry like \"star%%\" (lowercased, trailing-only wildcard); got args=%v", args)
	}
}

func TestQueryExecutor_NamePrefix_UsesEpisodeSortKeyForEpisodeScope(t *testing.T) {
	exec := &QueryExecutor{}
	access := AccessFilter{NamePrefix: "Pilot"}

	sql, args, err := exec.buildPreviewPageSQL(
		QueryDefinition{MediaScope: "episode", LibraryIDs: []int{2}},
		access,
		20,
		0,
		true,
	)
	if err != nil {
		t.Fatalf("buildPreviewPageSQL returned error: %v", err)
	}
	if !strings.Contains(sql, "mi.sort_key LIKE") {
		t.Fatalf("expected episode prefix to use projected sort_key; got %q", sql)
	}
	if strings.Contains(sql, "BTRIM(mi.sort_title)") {
		t.Fatalf("episode prefix should not recompute sort_title expression; got %q", sql)
	}
	if len(args) < 3 || args[2] != "pilot%" {
		t.Fatalf("expected episode and series library args followed by prefix arg; got %v", args)
	}
}

// TestBrowseFilters_NamePrefix_MatchesSortKeyOnly asserts that the prefix LIKE
// in BrowseRepository's WHERE clause uses the expression idx_media_items_sort_key
// (migration 102) indexes, so /Items?NameStartsWith=X stays index-backed on
// large catalogs, and that it does not also match LOWER(title): the letter rail
// must put "The Hobbit" (sort_title "Hobbit, The") under H only.
//
// Regression guard for the post-perf-overhaul code review (2026-05): the
// initial form was LOWER(COALESCE(NULLIF(BTRIM(sort_title),”), ”)) which
// fell back to ” instead of title and matched no index expression.
func TestBrowseFilters_NamePrefix_MatchesSortKeyOnly(t *testing.T) {
	_, where, _, earlyEmpty := filterWhereClauseForSource(
		BrowseFilters{NamePrefix: "Star"}, "media_items mi", "")
	if earlyEmpty {
		t.Fatalf("unexpected earlyEmpty for NamePrefix-only filter")
	}
	if !strings.Contains(where, "LOWER(COALESCE(NULLIF(BTRIM(mi.sort_title), ''), mi.title)) LIKE") {
		t.Fatalf("expected sort-key LIKE matching idx_media_items_sort_key; got %s", where)
	}
	if strings.Contains(where, "LOWER(mi.title) LIKE") {
		t.Fatalf("prefix must not match the raw title alongside the sort key; got %s", where)
	}
	// Reject the previous broken form that used '' as the COALESCE fallback.
	if strings.Contains(where, "BTRIM(mi.sort_title), ''), ''))") {
		t.Fatalf("sort key still uses '' fallback (defeats idx_media_items_sort_key); got %s", where)
	}
}

// TestEscapePrefixForLike checks that the helper lowercases input and escapes
// the LIKE wildcards (% _) and the escape character itself (\).
func TestEscapePrefixForLike(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Star", "star"},
		{"  Star  ", "star"},
		{"50%", `50\%`},
		{"a_b", `a\_b`},
		{`back\slash`, `back\\slash`},
		{`%_\`, `\%\_\\`},
	}
	for _, tc := range cases {
		got := escapePrefixForLike(tc.in)
		if got != tc.want {
			t.Errorf("escapePrefixForLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
