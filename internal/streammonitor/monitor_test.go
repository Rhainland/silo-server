package streammonitor

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/nodesessions"
)

func fakeFn(infos []nodesessions.SessionInfo) func(ctx context.Context) ([]nodesessions.SessionInfo, error) {
	return func(ctx context.Context) ([]nodesessions.SessionInfo, error) {
		return infos, nil
	}
}

func TestFuncSourceGrouping(t *testing.T) {
	infos := []nodesessions.SessionInfo{
		{SessionID: "s1", AuthUserID: 1, Type: "direct_play", StartedAt: "2026-07-04T10:00:00Z", LastServedAt: "2026-07-04T10:05:00Z"},
		{SessionID: "s2", AuthUserID: 1, Type: "transcode", StartedAt: "2026-07-04T10:01:00Z", LastServedAt: "2026-07-04T10:06:00Z"},
		{SessionID: "s3", AuthUserID: 2, Type: "remux", StartedAt: "2026-07-04T10:02:00Z", LastServedAt: "2026-07-04T10:07:00Z"},
		{SessionID: "s4", AuthUserID: 0, Type: "direct_play"}, // no user, no timestamps
	}

	snap, err := NewFuncSource(fakeFn(infos)).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if got := len(snap.Streams); got != 4 {
		t.Fatalf("Streams len = %d, want 4", got)
	}

	if got := snap.CountByUser(1); got != 2 {
		t.Errorf("CountByUser(1) = %d, want 2", got)
	}
	if got := snap.CountByUser(2); got != 1 {
		t.Errorf("CountByUser(2) = %d, want 1", got)
	}
	if got := snap.CountByUser(0); got != 1 {
		t.Errorf("CountByUser(0) = %d, want 1", got)
	}
	if got := snap.CountByUser(99); got != 0 {
		t.Errorf("CountByUser(99) = %d, want 0", got)
	}

	u1 := snap.StreamsForUser(1)
	if len(u1) != 2 {
		t.Fatalf("StreamsForUser(1) len = %d, want 2", len(u1))
	}
	for _, st := range u1 {
		if st.UserID != 1 {
			t.Errorf("StreamsForUser(1) returned stream with UserID %d", st.UserID)
		}
	}

	byUser := snap.ByUser()
	if len(byUser) != 3 {
		t.Fatalf("ByUser len = %d, want 3 (users 0,1,2)", len(byUser))
	}
	if len(byUser[1]) != 2 || len(byUser[2]) != 1 || len(byUser[0]) != 1 {
		t.Errorf("ByUser grouping wrong: %#v", map[int]int{0: len(byUser[0]), 1: len(byUser[1]), 2: len(byUser[2])})
	}
}

func TestTimestampParsing(t *testing.T) {
	infos := []nodesessions.SessionInfo{
		{SessionID: "s1", AuthUserID: 1, StartedAt: "2026-07-04T10:00:00Z", LastServedAt: "2026-07-04T10:05:00Z"},
		{SessionID: "s2", AuthUserID: 1, StartedAt: "not-a-time", LastServedAt: ""},
	}
	snap, err := NewFuncSource(fakeFn(infos)).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	bySession := map[string]LiveStream{}
	for _, st := range snap.Streams {
		bySession[st.SessionID] = st
	}

	want := time.Date(2026, 7, 4, 10, 5, 0, 0, time.UTC)
	if !bySession["s1"].LastServedAt.Equal(want) {
		t.Errorf("s1 LastServedAt = %v, want %v", bySession["s1"].LastServedAt, want)
	}
	if !bySession["s2"].StartedAt.IsZero() {
		t.Errorf("s2 StartedAt = %v, want zero (unparseable)", bySession["s2"].StartedAt)
	}
	if !bySession["s2"].LastServedAt.IsZero() {
		t.Errorf("s2 LastServedAt = %v, want zero (empty)", bySession["s2"].LastServedAt)
	}
}

func TestDedupeKeepsNewest(t *testing.T) {
	// Same SessionID observed on two nodes (proxy vs transcode node). Keep the
	// record with the most recent LastServedAt.
	infos := []nodesessions.SessionInfo{
		{SessionID: "dup", AuthUserID: 5, NodeName: "proxy", LastServedAt: "2026-07-04T10:00:00Z", BytesServed: 100},
		{SessionID: "dup", AuthUserID: 5, NodeName: "transcode", LastServedAt: "2026-07-04T10:09:00Z", BytesServed: 900},
		{SessionID: "other", AuthUserID: 5, NodeName: "proxy", LastServedAt: "2026-07-04T10:03:00Z"},
	}
	snap, err := NewFuncSource(fakeFn(infos)).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if got := len(snap.Streams); got != 2 {
		t.Fatalf("Streams len = %d, want 2 (deduped)", got)
	}
	if got := snap.CountByUser(5); got != 2 {
		t.Errorf("CountByUser(5) = %d, want 2", got)
	}

	var dup LiveStream
	found := false
	for _, st := range snap.Streams {
		if st.SessionID == "dup" {
			dup = st
			found = true
		}
	}
	if !found {
		t.Fatal("deduped session 'dup' not present")
	}
	if dup.NodeName != "transcode" {
		t.Errorf("dedupe kept NodeName %q, want transcode (newest LastServedAt)", dup.NodeName)
	}
	if dup.BytesServed != 900 {
		t.Errorf("dedupe kept BytesServed %d, want 900", dup.BytesServed)
	}
}

func TestMergeStreamsDirect(t *testing.T) {
	// Direct unit test of the merge helper, mirroring RedisSource dedupe.
	in := []LiveStream{
		{SessionID: "a", NodeName: "n1", LastServedAt: time.Unix(100, 0)},
		{SessionID: "a", NodeName: "n2", LastServedAt: time.Unix(200, 0)},
		{SessionID: "b", NodeName: "n1", LastServedAt: time.Unix(150, 0)},
	}
	out := mergeStreams(in)
	if len(out) != 2 {
		t.Fatalf("mergeStreams len = %d, want 2", len(out))
	}
	for _, st := range out {
		if st.SessionID == "a" && st.NodeName != "n2" {
			t.Errorf("merge kept %q for session a, want n2 (newest)", st.NodeName)
		}
	}
}

func TestMergeStreamsCarriesOwnershipForward(t *testing.T) {
	// The transcode node's own start record has no resolved owner (UserID 0) and
	// can be the freshest copy of a session. Taking it wholesale would bucket the
	// session under user 0, which the enforcer skips — exempting it from the cap.
	// The merge must recover the owner from the proxy's (staler) owned record.
	in := []LiveStream{
		{SessionID: "s", NodeName: "proxy", UserID: 42, ProfileID: "p1", MediaFileID: 7, LastServedAt: time.Unix(100, 0)},
		{SessionID: "s", NodeName: "transcode", UserID: 0, LastServedAt: time.Unix(200, 0)},
	}
	out := mergeStreams(in)
	if len(out) != 1 {
		t.Fatalf("mergeStreams len = %d, want 1", len(out))
	}
	got := out[0]
	if got.NodeName != "transcode" {
		t.Errorf("kept NodeName %q, want transcode (freshest)", got.NodeName)
	}
	if got.UserID != 42 {
		t.Errorf("UserID = %d, want 42 (recovered from the proxy record)", got.UserID)
	}
	if got.ProfileID != "p1" || got.MediaFileID != 7 {
		t.Errorf("ownership not fully recovered: ProfileID=%q MediaFileID=%d", got.ProfileID, got.MediaFileID)
	}
}

func TestToLiveStreamCarriesRouteAndClient(t *testing.T) {
	// The normalized view must surface route (native vs jellycompat) and client
	// identity so the monitor is first-class, not just session id + method.
	info := nodesessions.SessionInfo{
		SessionID:  "s1",
		AuthUserID: 9,
		Type:       "transcode",
		Route:      "jellycompat",
		ClientIP:   "203.0.113.7",
		ClientName: "Infuse",
		Position:   61.5,
		HWAccel:    "vaapi",
	}
	got := toLiveStream(info)
	if got.Route != "jellycompat" || got.ClientIP != "203.0.113.7" || got.ClientName != "Infuse" {
		t.Fatalf("route/client not carried: %+v", got)
	}
	if got.Position != 61.5 || got.HWAccel != "vaapi" {
		t.Fatalf("position/hwaccel not carried: %+v", got)
	}
}

func TestMergeStreamsBackfillsAttribution(t *testing.T) {
	// A fresher-but-thinner record (e.g. an ownerless transcode-node start record)
	// must not drop route/client that a staler record for the same session carries.
	in := []LiveStream{
		{SessionID: "s", NodeName: "proxy", UserID: 5, Route: "native", ClientName: "SiloTV", ClientIP: "10.0.0.9", LastServedAt: time.Unix(100, 0)},
		{SessionID: "s", NodeName: "transcode", UserID: 0, LastServedAt: time.Unix(200, 0)},
	}
	out := mergeStreams(in)
	if len(out) != 1 {
		t.Fatalf("merge len = %d, want 1", len(out))
	}
	got := out[0]
	if got.NodeName != "transcode" {
		t.Errorf("kept NodeName %q, want transcode (freshest)", got.NodeName)
	}
	if got.UserID != 5 || got.Route != "native" || got.ClientName != "SiloTV" || got.ClientIP != "10.0.0.9" {
		t.Errorf("attribution not backfilled from staler record: %+v", got)
	}
}

func TestFuncSourceNilFn(t *testing.T) {
	snap, err := NewFuncSource(nil).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Streams) != 0 {
		t.Errorf("nil fn Streams len = %d, want 0", len(snap.Streams))
	}
}

func TestRedisSourceNilClient(t *testing.T) {
	snap, err := NewRedisSource(nil).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Streams) != 0 {
		t.Errorf("nil client Streams len = %d, want 0", len(snap.Streams))
	}
}

// TestDedupeSessionInfos mirrors the mergeStreams rules on the raw SessionInfo
// shape used by the admin session list: one row per session id, freshest copy
// wins, a resolved owner and missing attribution are carried across the merge
// so the operator view matches the enforcer's picture (no double-counted
// streams, no user-0 rows when any copy knows the owner).
func TestDedupeSessionInfos(t *testing.T) {
	newer := time.Now().UTC().Format(time.RFC3339)
	older := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	infos := []nodesessions.SessionInfo{
		{SessionID: "s1", NodeName: "central", AuthUserID: 7, ProfileID: "p1", MediaFileID: 42,
			Route: "native", ClientName: "SiloTV", Position: 130, LastServedAt: older},
		// The same stream, seen from the edge serving it: freshest but ownerless.
		{SessionID: "s1", NodeName: "edge-1", LastServedAt: newer, BytesServed: 9000},
		{SessionID: "s2", NodeName: "edge-1", AuthUserID: 8, LastServedAt: newer},
	}

	out := DedupeSessionInfos(infos)
	if len(out) != 2 {
		t.Fatalf("dedupe len = %d, want 2", len(out))
	}
	byID := make(map[string]nodesessions.SessionInfo, len(out))
	for _, info := range out {
		byID[info.SessionID] = info
	}
	s1, ok := byID["s1"]
	if !ok {
		t.Fatalf("s1 missing from dedupe output")
	}
	if s1.NodeName != "edge-1" || s1.BytesServed != 9000 {
		t.Errorf("s1 freshest copy not kept: %+v", s1)
	}
	if s1.AuthUserID != 7 || s1.ProfileID != "p1" || s1.MediaFileID != 42 {
		t.Errorf("s1 owner not carried across merge: %+v", s1)
	}
	if s1.Route != "native" || s1.ClientName != "SiloTV" || s1.Position != 130 {
		t.Errorf("s1 attribution not backfilled: %+v", s1)
	}
	if s2 := byID["s2"]; s2.AuthUserID != 8 {
		t.Errorf("s2 mangled by dedupe: %+v", s2)
	}
}
