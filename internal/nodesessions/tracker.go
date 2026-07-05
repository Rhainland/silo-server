package nodesessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// KeyPrefix is the Redis key namespace for node session records
	// (silo:sessions:{nodeHash}:{sessionID}). Exported so readers of the
	// monitoring picture (streammonitor, the admin session list) SCAN the same
	// namespace this tracker writes instead of duplicating the literal.
	KeyPrefix = "silo:sessions:"

	sessionTTL = 60 * time.Second
	refreshInt = 30 * time.Second
)

// SessionInfo represents an active streaming session stored in Redis.
type SessionInfo struct {
	SessionID   string `json:"session_id"`
	NodeURL     string `json:"node_url"`
	NodeName    string `json:"node_name"`
	UserID      string `json:"user_id,omitempty"`
	MediaItemID string `json:"media_item_id,omitempty"`
	MediaTitle  string `json:"media_title,omitempty"`
	Type        string `json:"type"` // "direct_play", "remux", "transcode"
	CodecVideo  string `json:"codec_video,omitempty"`
	CodecAudio  string `json:"codec_audio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	HWAccel     string `json:"hw_accel,omitempty"`
	StartedAt   string `json:"started_at"`

	// LastServedAt / BytesServed are the authoritative, server-observed liveness
	// signals: they are refreshed only when the node actually serves bytes for this
	// session (a segment written, a direct-play/remux pour advancing), never from a
	// client progress report. A "quiet" stream that keeps pulling stays fresh here;
	// one that stops pulling ages out. This is the anti-abuse monitoring surface.
	LastServedAt string `json:"last_served_at,omitempty"`
	BytesServed  int64  `json:"bytes_served,omitempty"`

	// AuthUserID / ProfileID / MediaFileID are the numeric ownership keys the
	// node copies from the verified stream token. They enrich the live admin
	// "active streams" view (served by SCANning these records) so it can answer
	// *who* is watching *what* on each node, not just session id + node + type;
	// the string UserID/MediaItemID/MediaTitle fields remain the display labels.
	AuthUserID  int    `json:"auth_user_id,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	MediaFileID int    `json:"media_file_id,omitempty"`

	// Route is the origin protocol ("native" | "jellycompat"). Type is the play
	// method (direct_play/remux/transcode); Route is orthogonal — a jellycompat
	// stream and a native one share a Type but differ in Route. Client* are
	// best-effort viewer identity for oversight and abuse heuristics (e.g. one
	// re-streaming client fanning out to many viewers). Position is the last
	// known playback position in seconds (secondary timing).
	Route      string  `json:"route,omitempty"`
	ClientIP   string  `json:"client_ip,omitempty"`
	ClientName string  `json:"client_name,omitempty"`
	Position   float64 `json:"position,omitempty"`
}

// Tracker manages session lifecycle in Redis for a single node.
type Tracker struct {
	rdb      *redis.Client
	nodeURL  string
	nodeName string
	nodeType string
	nodeHash string // first 8 chars of SHA-256 of nodeURL

	mu       sync.Mutex
	sessions map[string]struct{}    // set of active (explicitly tracked) session IDs
	touched  map[string]time.Time   // ephemeral sessions by last-activity time
	records  map[string]SessionInfo // last-written record per session, for enriched refresh
	bytes    map[string]int64       // cumulative bytes served per session (monitoring only)
}

// NewTracker creates a session tracker for the given node.
// rdb may be nil, in which case all operations are no-ops.
func NewTracker(rdb *redis.Client, nodeURL, nodeName, nodeType string) *Tracker {
	h := sha256.Sum256([]byte(nodeURL))
	return &Tracker{
		rdb:      rdb,
		nodeURL:  nodeURL,
		nodeName: nodeName,
		nodeType: nodeType,
		nodeHash: hex.EncodeToString(h[:4]), // 8 hex chars
		sessions: make(map[string]struct{}),
		touched:  make(map[string]time.Time),
		records:  make(map[string]SessionInfo),
		bytes:    make(map[string]int64),
	}
}

// redisKey returns the full Redis key for a session.
func (tr *Tracker) redisKey(sessionID string) string {
	return KeyPrefix + tr.nodeHash + ":" + sessionID
}

// NodeHash returns the node's hash prefix used in Redis keys.
func (tr *Tracker) NodeHash() string {
	return tr.nodeHash
}

// NodeURL returns the node's URL.
func (tr *Tracker) NodeURL() string {
	return tr.nodeURL
}

// NodeName returns the node's display name.
func (tr *Tracker) NodeName() string {
	return tr.nodeName
}

// ActiveCount returns the number of active sessions tracked by this node,
// including ephemeral sessions touched within the session TTL.
func (tr *Tracker) ActiveCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	now := time.Now()
	count := len(tr.sessions)
	for id, last := range tr.touched {
		if _, dup := tr.sessions[id]; dup {
			continue
		}
		if now.Sub(last) <= sessionTTL {
			count++
		}
	}
	return count
}

// Snapshot returns a copy of the currently-known session records on this node
// (including ephemeral ones still within their TTL), stamped with the latest
// LastServedAt/BytesServed. Used for the node /status view; central aggregates
// the equivalent picture by reading Redis.
func (tr *Tracker) Snapshot() []SessionInfo {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	now := time.Now()
	out := make([]SessionInfo, 0, len(tr.records))
	for id, rec := range tr.records {
		// Session-backed entries are always live until Remove; only ephemeral
		// (non-session) entries age out by idle timeout.
		if _, isSession := tr.sessions[id]; !isSession {
			if last, ok := tr.touched[id]; ok && now.Sub(last) > sessionTTL {
				continue
			}
		}
		out = append(out, tr.enrichLocked(id, rec))
	}
	return out
}

// enrichLocked stamps a record with the latest observed liveness/bytes. Caller
// holds tr.mu.
func (tr *Tracker) enrichLocked(id string, rec SessionInfo) SessionInfo {
	if last, ok := tr.touched[id]; ok {
		rec.LastServedAt = last.UTC().Format(time.RFC3339)
	}
	if b, ok := tr.bytes[id]; ok {
		rec.BytesServed = b
	}
	return rec
}

// Track registers an active session in Redis with a TTL.
func (tr *Tracker) Track(ctx context.Context, info SessionInfo) {
	if tr.rdb == nil {
		return
	}
	now := time.Now()
	if info.LastServedAt == "" {
		info.LastServedAt = now.UTC().Format(time.RFC3339)
	}

	// Explicitly-tracked sessions (direct play / remux) live in tr.sessions for
	// the whole connection. touched holds their last-served time so LastServedAt
	// reflects real activity; refreshAll only idle-prunes ephemeral (non-session)
	// touched entries, so a long quiet pour is never expired while its connection
	// is open.
	tr.mu.Lock()
	tr.preserveStartedAtLocked(&info)
	tr.sessions[info.SessionID] = struct{}{}
	tr.records[info.SessionID] = info
	tr.touched[info.SessionID] = now
	enriched := tr.enrichLocked(info.SessionID, info)
	tr.mu.Unlock()

	data, err := json.Marshal(enriched)
	if err != nil {
		slog.Debug("session track marshal failed", "error", err)
		return
	}
	if err := tr.rdb.Set(ctx, tr.redisKey(info.SessionID), data, sessionTTL).Err(); err != nil {
		slog.Debug("session track set failed", "error", err, "session", info.SessionID)
	}
}

// Touch registers or refreshes an ephemeral session that has no explicit end,
// such as HLS manifest/segment fetches flowing through a proxy. The session is
// written to Redis on first touch and drops out of the active count after
// sessionTTL without further touches (pruned by the refresh loop). Subsequent
// touches update LastServedAt in memory; the body is re-flushed on the refresh
// tick so the monitoring record reflects real serve activity.
func (tr *Tracker) Touch(ctx context.Context, info SessionInfo) {
	if tr.rdb == nil {
		return
	}
	now := time.Now()
	tr.mu.Lock()
	tr.preserveStartedAtLocked(&info)
	_, known := tr.touched[info.SessionID]
	tr.touched[info.SessionID] = now
	tr.records[info.SessionID] = info
	enriched := tr.enrichLocked(info.SessionID, info)
	tr.mu.Unlock()
	if known {
		return
	}

	data, err := json.Marshal(enriched)
	if err != nil {
		slog.Debug("session touch marshal failed", "error", err)
		return
	}
	if err := tr.rdb.Set(ctx, tr.redisKey(info.SessionID), data, sessionTTL).Err(); err != nil {
		slog.Debug("session touch set failed", "error", err, "session", info.SessionID)
	}
}

// preserveStartedAtLocked keeps the first-seen StartedAt when a record is
// re-written for a session we already track. Track re-fires on every range
// reconnect and Touch stamps a fresh sessionInfo per segment fetch; without
// this, StartedAt degrades to "time of last request", corrupting the admin
// view and the enforcer's StartedAt tie-break in victim selection. Caller
// holds tr.mu.
func (tr *Tracker) preserveStartedAtLocked(info *SessionInfo) {
	if prev, ok := tr.records[info.SessionID]; ok && prev.StartedAt != "" {
		info.StartedAt = prev.StartedAt
	}
}

// AddBytes records that n bytes were served for a session and marks it as active
// now. Cheap and lock-guarded; the accumulated value is flushed to Redis on the
// refresh tick. Bytes attribution is best-effort monitoring and never gates the
// hot path. Bytes for a session with no live record are dropped: a late tally
// arriving after the record was idle-pruned (or removed) must not recreate a
// bytes entry nothing will ever clean up — that was a slow permanent leak on
// busy edges.
func (tr *Tracker) AddBytes(sessionID string, n int64) {
	if tr.rdb == nil || n <= 0 {
		return
	}
	tr.mu.Lock()
	if _, known := tr.records[sessionID]; known {
		tr.bytes[sessionID] += n
		// Mark real serve activity so LastServedAt advances for every session
		// type, including long direct-play/remux pours.
		tr.touched[sessionID] = time.Now()
	}
	tr.mu.Unlock()
}

// MarkServed records real serve activity for a session without byte
// attribution, advancing LastServedAt (flushed on the refresh tick). Used by
// the transcode node's own serve paths, where wrapping ServeFile for byte
// counting would cost the sendfile fast path for a signal the fronting proxy
// already measures — without it the node record's LastServedAt stays frozen at
// start time, which starves the enforcer's freshness ordering and the admin
// view of the node-side truth.
func (tr *Tracker) MarkServed(sessionID string) {
	if tr == nil || tr.rdb == nil {
		return
	}
	tr.mu.Lock()
	if _, known := tr.records[sessionID]; known {
		tr.touched[sessionID] = time.Now()
	}
	tr.mu.Unlock()
}

// Remove deletes a session from Redis and the in-memory set.
func (tr *Tracker) Remove(ctx context.Context, sessionID string) {
	if tr.rdb == nil {
		return
	}
	tr.mu.Lock()
	delete(tr.sessions, sessionID)
	delete(tr.touched, sessionID)
	delete(tr.records, sessionID)
	delete(tr.bytes, sessionID)
	tr.mu.Unlock()

	if err := tr.rdb.Del(ctx, tr.redisKey(sessionID)).Err(); err != nil {
		slog.Debug("session remove failed", "error", err, "session", sessionID)
	}
}

// Cleanup deletes all session keys for this node. Called on graceful shutdown.
func (tr *Tracker) Cleanup(ctx context.Context) {
	if tr.rdb == nil {
		return
	}
	tr.mu.Lock()
	ids := make([]string, 0, len(tr.sessions)+len(tr.touched))
	for id := range tr.sessions {
		ids = append(ids, id)
	}
	for id := range tr.touched {
		if _, dup := tr.sessions[id]; !dup {
			ids = append(ids, id)
		}
	}
	tr.sessions = make(map[string]struct{})
	tr.touched = make(map[string]time.Time)
	tr.records = make(map[string]SessionInfo)
	tr.bytes = make(map[string]int64)
	tr.mu.Unlock()

	if len(ids) == 0 {
		return
	}

	pipe := tr.rdb.Pipeline()
	for _, id := range ids {
		pipe.Del(ctx, tr.redisKey(id))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Debug("session cleanup pipeline failed", "error", err)
	}
}

// StartRefresh starts a background goroutine that refreshes TTLs for all
// active sessions every 30 seconds. Stops when ctx is cancelled.
func (tr *Tracker) StartRefresh(ctx context.Context) {
	if tr.rdb == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(refreshInt)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tr.refreshAll(ctx)
			}
		}
	}()
}

func (tr *Tracker) refreshAll(ctx context.Context) {
	now := time.Now()
	type flush struct {
		id   string
		data []byte
	}
	tr.mu.Lock()
	ids := make([]string, 0, len(tr.sessions))
	for id := range tr.sessions {
		ids = append(ids, id)
	}
	for id, last := range tr.touched {
		_, isSession := tr.sessions[id]
		if !isSession && now.Sub(last) > sessionTTL {
			// Idle ephemeral session: stop refreshing and let the Redis key
			// expire on its own. Session-backed entries (direct/remux) are pruned
			// on Remove, never by idle timeout, so a quiet-but-open pour stays live.
			delete(tr.touched, id)
			delete(tr.records, id)
			delete(tr.bytes, id)
			continue
		}
		if !isSession {
			ids = append(ids, id)
		}
	}
	flushes := make([]flush, 0, len(ids))
	for _, id := range ids {
		rec, ok := tr.records[id]
		if !ok {
			continue
		}
		data, err := json.Marshal(tr.enrichLocked(id, rec))
		if err != nil {
			continue
		}
		flushes = append(flushes, flush{id: id, data: data})
	}
	tr.mu.Unlock()

	if len(flushes) == 0 {
		return
	}

	// Re-SET (not just EXPIRE) so LastServedAt/BytesServed stay current in the
	// monitoring record. 30s cadence keeps this cheap.
	pipe := tr.rdb.Pipeline()
	for _, f := range flushes {
		pipe.Set(ctx, tr.redisKey(f.id), f.data, sessionTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Debug("session refresh pipeline failed", "error", err)
	}
}
