// Package streammonitor produces a normalized "live streams" snapshot grouped by
// user from the authoritative server-side monitoring records written by
// internal/nodesessions. It backs an async enforcement loop and admin views.
//
// EXISTENCE is server-observed on every path and never gated by a client report,
// so a "hidden stream" (a disguised client that pulls bytes but withholds
// progress) is always counted:
//   - Edge (multi-node): a record exists in Redis for the whole connection
//     (direct/remux Track..Remove) or while segments are pulled (transcode Touch),
//     and BytesServed/LastServedAt advance only on real bytes.
//   - Integrated: the FuncSource reflects SessionManager.AllSessions(), and a
//     session is unreapable while it holds an in-flight transport marker
//     (BeginTransport/EndTransport around every byte pour) — no client progress
//     required to stay visible.
//
// TIMING is a secondary signal. On the edge, LastServedAt is purely byte-observed.
// In integrated mode LastServedAt is mapped from SessionManager.LastActivityAt,
// which client progress reports also advance; that is acceptable because it is
// used only to order over-cap victims (selectVictims), never to decide whether a
// stream exists or is counted. See internal/nodesessions for record production.
package streammonitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Silo-Server/silo-server/internal/nodesessions"
)

// sessionKeyPrefix is the Redis key prefix under which nodesessions.Tracker
// stores its SessionInfo records — the tracker's own exported constant, so the
// write and read sides can never drift.
const sessionKeyPrefix = nodesessions.KeyPrefix

// scanCount is the COUNT hint passed to SCAN. It bounds the amount of work per
// round trip while keeping the number of round trips reasonable.
const scanCount = 256

// LiveStream is a normalized view of a single active streaming session.
type LiveStream struct {
	SessionID    string
	UserID       int // from SessionInfo.AuthUserID
	ProfileID    string
	NodeName     string
	NodeURL      string
	Type         string // play method: direct_play | remux | transcode
	Route        string // origin protocol: native | jellycompat
	MediaFileID  int
	ClientIP     string
	ClientName   string
	Position     float64 // last known playback position (seconds); secondary timing
	HWAccel      string
	LastServedAt time.Time // parsed from SessionInfo.LastServedAt (zero if absent)
	BytesServed  int64
	StartedAt    time.Time // parsed from SessionInfo.StartedAt (zero if unparseable)
}

// Snapshot is a point-in-time picture of the live streams.
type Snapshot struct {
	Streams []LiveStream
}

// CountByUser returns the number of live streams owned by userID.
func (s Snapshot) CountByUser(userID int) int {
	n := 0
	for _, st := range s.Streams {
		if st.UserID == userID {
			n++
		}
	}
	return n
}

// StreamsForUser returns the live streams owned by userID.
func (s Snapshot) StreamsForUser(userID int) []LiveStream {
	out := make([]LiveStream, 0)
	for _, st := range s.Streams {
		if st.UserID == userID {
			out = append(out, st)
		}
	}
	return out
}

// ByUser groups the live streams by owning user id.
func (s Snapshot) ByUser() map[int][]LiveStream {
	out := make(map[int][]LiveStream)
	for _, st := range s.Streams {
		out[st.UserID] = append(out[st.UserID], st)
	}
	return out
}

// Source yields the current live picture.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// MultiSource unions several sources into one snapshot, de-duplicating sessions
// that appear in more than one backend (e.g. a session held both in the central
// session manager and mirrored by an edge's Redis record) by keeping the most-
// recently-served copy. A source that errors is skipped (logged), so one
// unavailable backend never blinds the enforcer to the others. This is what lets
// the enforcer see BOTH locally-served (integrated) and edge-served (multi-node)
// streams regardless of whether Redis is configured.
type MultiSource struct {
	sources []Source
}

// NewMultiSource builds a union source over the given sources (nil entries are
// ignored).
func NewMultiSource(sources ...Source) *MultiSource {
	return &MultiSource{sources: sources}
}

// Snapshot merges every sub-source's snapshot, de-duplicated by session id.
func (m *MultiSource) Snapshot(ctx context.Context) (Snapshot, error) {
	var all []LiveStream
	for _, src := range m.sources {
		if src == nil {
			continue
		}
		snap, err := src.Snapshot(ctx)
		if err != nil {
			slog.Warn("streammonitor: source snapshot failed; skipping", "error", err)
			continue
		}
		all = append(all, snap.Streams...)
	}
	return Snapshot{Streams: mergeStreams(all)}, nil
}

// toLiveStream converts a nodesessions.SessionInfo into a LiveStream, parsing
// the RFC3339 timestamps and tolerating empty/unparseable values (which map to
// the zero time).
func toLiveStream(info nodesessions.SessionInfo) LiveStream {
	return LiveStream{
		SessionID:    info.SessionID,
		UserID:       info.AuthUserID,
		ProfileID:    info.ProfileID,
		NodeName:     info.NodeName,
		NodeURL:      info.NodeURL,
		Type:         info.Type,
		Route:        info.Route,
		MediaFileID:  info.MediaFileID,
		ClientIP:     info.ClientIP,
		ClientName:   info.ClientName,
		Position:     info.Position,
		HWAccel:      info.HWAccel,
		LastServedAt: parseTime(info.LastServedAt),
		BytesServed:  info.BytesServed,
		StartedAt:    parseTime(info.StartedAt),
	}
}

// parseTime parses an RFC3339 timestamp, returning the zero time for empty or
// unparseable input.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		slog.Debug("streammonitor: unparseable timestamp", "value", s, "error", err)
		return time.Time{}
	}
	return t
}

// mergeStreams collapses records that share a SessionID (the same session can be
// tracked by more than one node — e.g. a proxy record and a transcode-node
// record), keeping the one with the most recent LastServedAt. Records with a
// distinct SessionID are all retained. The relative order of the kept records is
// not guaranteed.
//
// Ownership and attribution are carried forward independently of the freshness
// pick: the transcode node's own start record has no resolved owner (UserID 0)
// and thinner attribution, while the proxy record fronting it does. If the
// ownerless node record happened to be the freshest copy, taking it wholesale
// would bucket the session under user 0 (which the enforcer skips — silently
// exempting it from the concurrency cap) and drop route/client detail from the
// monitor view. So a merged record adopts a resolved owner from either candidate
// and backfills any empty attribution/display field from the other copy.
func mergeStreams(streams []LiveStream) []LiveStream {
	bySession := make(map[string]LiveStream, len(streams))
	for _, st := range streams {
		existing, ok := bySession[st.SessionID]
		if !ok {
			bySession[st.SessionID] = st
			continue
		}
		winner := existing
		other := st
		if st.LastServedAt.After(existing.LastServedAt) {
			winner, other = st, existing
		}
		// If the freshest copy is ownerless, adopt a resolved owner from either
		// candidate so the session is still attributed (and enforced) correctly.
		if winner.UserID <= 0 {
			for _, cand := range []LiveStream{existing, st} {
				if cand.UserID > 0 {
					winner.UserID = cand.UserID
					winner.ProfileID = cand.ProfileID
					winner.MediaFileID = cand.MediaFileID
					break
				}
			}
		}
		// Backfill display/attribution fields the freshest copy lacks so the
		// merged record is as complete as possible for the monitor view.
		if winner.Route == "" {
			winner.Route = other.Route
		}
		if winner.ClientIP == "" {
			winner.ClientIP = other.ClientIP
		}
		if winner.ClientName == "" {
			winner.ClientName = other.ClientName
		}
		if winner.HWAccel == "" {
			winner.HWAccel = other.HWAccel
		}
		if winner.Position == 0 {
			winner.Position = other.Position
		}
		bySession[st.SessionID] = winner
	}
	out := make([]LiveStream, 0, len(bySession))
	for _, st := range bySession {
		out = append(out, st)
	}
	return out
}

// DedupeSessionInfos collapses raw monitoring records that share a SessionID,
// applying the same rules as mergeStreams — keep the most-recently-served copy,
// carry a resolved owner forward, backfill missing attribution — but preserving
// the SessionInfo shape for surfaces whose wire format IS the raw record (the
// admin session list, which unions Redis edge records with the in-process
// integrated sessions and would otherwise show the same stream twice). Kept as
// a sibling of mergeStreams rather than a shared generic because mergeStreams
// operates on the parsed LiveStream form; keep the two rule sets in sync.
func DedupeSessionInfos(infos []nodesessions.SessionInfo) []nodesessions.SessionInfo {
	bySession := make(map[string]nodesessions.SessionInfo, len(infos))
	order := make([]string, 0, len(infos))
	for _, in := range infos {
		existing, ok := bySession[in.SessionID]
		if !ok {
			bySession[in.SessionID] = in
			order = append(order, in.SessionID)
			continue
		}
		winner, other := existing, in
		if parseTime(in.LastServedAt).After(parseTime(existing.LastServedAt)) {
			winner, other = in, existing
		}
		if winner.AuthUserID <= 0 && other.AuthUserID > 0 {
			winner.AuthUserID = other.AuthUserID
			winner.ProfileID = other.ProfileID
			winner.MediaFileID = other.MediaFileID
		}
		if winner.Route == "" {
			winner.Route = other.Route
		}
		if winner.ClientIP == "" {
			winner.ClientIP = other.ClientIP
		}
		if winner.ClientName == "" {
			winner.ClientName = other.ClientName
		}
		if winner.HWAccel == "" {
			winner.HWAccel = other.HWAccel
		}
		if winner.Position == 0 {
			winner.Position = other.Position
		}
		bySession[in.SessionID] = winner
	}
	out := make([]nodesessions.SessionInfo, 0, len(bySession))
	for _, id := range order {
		out = append(out, bySession[id])
	}
	return out
}

// RedisSource reads silo:sessions:* records, producing the multi-node
// authoritative picture.
type RedisSource struct {
	rdb *redis.Client
}

// NewRedisSource creates a RedisSource backed by rdb.
func NewRedisSource(rdb *redis.Client) *RedisSource {
	return &RedisSource{rdb: rdb}
}

// Snapshot SCANs every silo:sessions:* record, decodes it, and returns the
// deduped live picture. The same session appearing on multiple nodes is
// collapsed to the record with the most recent LastServedAt.
func (r *RedisSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if r.rdb == nil {
		return Snapshot{Streams: []LiveStream{}}, nil
	}

	// Fully iterate the cursor to collect matching keys. SCAN (not KEYS) keeps
	// this non-blocking against a large keyspace.
	var keys []string
	var cursor uint64
	for {
		batch, next, err := r.rdb.Scan(ctx, cursor, sessionKeyPrefix+"*", scanCount).Result()
		if err != nil {
			return Snapshot{}, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		return Snapshot{Streams: []LiveStream{}}, nil
	}

	vals, err := r.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return Snapshot{}, err
	}

	streams := make([]LiveStream, 0, len(vals))
	for i, v := range vals {
		if v == nil {
			// Key expired between SCAN and MGET; skip.
			continue
		}
		var raw string
		switch val := v.(type) {
		case string:
			raw = val
		case []byte:
			raw = string(val)
		default:
			slog.Debug("streammonitor: unexpected redis value type", "key", keys[i])
			continue
		}
		var info nodesessions.SessionInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			slog.Debug("streammonitor: unmarshal session record failed", "key", keys[i], "error", err)
			continue
		}
		streams = append(streams, toLiveStream(info))
	}

	return Snapshot{Streams: mergeStreams(streams)}, nil
}

// FuncSource adapts an in-process provider (integrated single-node) that returns
// the local tracker records.
type FuncSource struct {
	fn func(ctx context.Context) ([]nodesessions.SessionInfo, error)
}

// NewFuncSource creates a FuncSource backed by fn.
func NewFuncSource(fn func(ctx context.Context) ([]nodesessions.SessionInfo, error)) *FuncSource {
	return &FuncSource{fn: fn}
}

// Snapshot invokes the wrapped provider and returns the deduped live picture.
func (f *FuncSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if f.fn == nil {
		return Snapshot{Streams: []LiveStream{}}, nil
	}
	infos, err := f.fn(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	streams := make([]LiveStream, 0, len(infos))
	for _, info := range infos {
		streams = append(streams, toLiveStream(info))
	}
	return Snapshot{Streams: mergeStreams(streams)}, nil
}
