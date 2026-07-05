package streamenforcer

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/streammonitor"
)

type fakeSource struct {
	snap streammonitor.Snapshot
	err  error
}

func (f fakeSource) Snapshot(context.Context) (streammonitor.Snapshot, error) {
	return f.snap, f.err
}

type fakeRevoker struct {
	revoked []string
	err     error
}

func (f *fakeRevoker) RevokeSessionFor(_ context.Context, sessionID, _ string, _ time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.revoked = append(f.revoked, sessionID)
	return nil
}

// stream builds a LiveStream whose StartedAt and LastServedAt are both ts.
func stream(id string, user int, ts time.Time) streammonitor.LiveStream {
	return streammonitor.LiveStream{SessionID: id, UserID: user, StartedAt: ts, LastServedAt: ts}
}

// served builds a LiveStream started at `started` but last served at `lastServed`
// (to model a ghost: old activity) — StartedAt independent from real liveness.
func served(id string, user int, started, lastServed time.Time) streammonitor.LiveStream {
	return streammonitor.LiveStream{SessionID: id, UserID: user, StartedAt: started, LastServedAt: lastServed}
}

func TestEvaluateOnce(t *testing.T) {
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	limit3 := func(context.Context, int) (int, error) { return 3, nil }

	tests := []struct {
		name       string
		streams    []streammonitor.LiveStream
		limits     LimitFunc
		wantRevoke []string
	}{
		{
			name:       "under cap: no revokes",
			streams:    []streammonitor.LiveStream{stream("a", 1, base), stream("b", 1, base.Add(time.Minute))},
			limits:     limit3,
			wantRevoke: nil,
		},
		{
			name: "over cap: least-recently-served trimmed, freshest kept",
			streams: []streammonitor.LiveStream{
				stream("stale", 1, base),
				stream("old", 1, base.Add(1*time.Minute)),
				stream("keep1", 1, base.Add(2*time.Minute)),
				stream("keep2", 1, base.Add(3*time.Minute)),
				stream("keep3", 1, base.Add(4*time.Minute)),
			},
			limits:     limit3,
			wantRevoke: []string{"stale", "old"},
		},
		{
			name: "ghost trimmed, fresh reconnect survives",
			// Two fresh (served just now) and two ghosts (served long ago), limit 2.
			streams: []streammonitor.LiveStream{
				served("ghost1", 1, base, base),
				served("ghost2", 1, base.Add(time.Second), base.Add(time.Second)),
				served("fresh1", 1, base.Add(10*time.Minute), base.Add(10*time.Minute)),
				served("fresh2", 1, base.Add(10*time.Minute), base.Add(11*time.Minute)),
			},
			limits:     func(context.Context, int) (int, error) { return 2, nil },
			wantRevoke: []string{"ghost1", "ghost2"},
		},
		{
			name:       "limit lookup error: fail open",
			streams:    []streammonitor.LiveStream{stream("a", 1, base), stream("b", 1, base), stream("c", 1, base), stream("d", 1, base)},
			limits:     func(context.Context, int) (int, error) { return 0, errors.New("db down") },
			wantRevoke: nil,
		},
		{
			name:       "unlimited (limit<=0): no revokes",
			streams:    []streammonitor.LiveStream{stream("a", 1, base), stream("b", 1, base), stream("c", 1, base)},
			limits:     func(context.Context, int) (int, error) { return 0, nil },
			wantRevoke: nil,
		},
		{
			name:       "user 0 never enforced",
			streams:    []streammonitor.LiveStream{stream("a", 0, base), stream("b", 0, base), stream("c", 0, base), stream("d", 0, base)},
			limits:     limit3,
			wantRevoke: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rev := &fakeRevoker{}
			e := New(fakeSource{snap: streammonitor.Snapshot{Streams: tc.streams}}, tc.limits, rev, 0)
			if err := e.EvaluateOnce(context.Background()); err != nil {
				t.Fatalf("EvaluateOnce: %v", err)
			}
			got := append([]string{}, rev.revoked...)
			sort.Strings(got)
			want := append([]string{}, tc.wantRevoke...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("revoked = %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("revoked = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestEvaluateOnceSourceError(t *testing.T) {
	rev := &fakeRevoker{}
	e := New(fakeSource{err: errors.New("scan failed")}, func(context.Context, int) (int, error) { return 1, nil }, rev, 0)
	if err := e.EvaluateOnce(context.Background()); err == nil {
		t.Fatal("expected error from source")
	}
	if len(rev.revoked) != 0 {
		t.Fatalf("expected no revokes on source error, got %v", rev.revoked)
	}
}
