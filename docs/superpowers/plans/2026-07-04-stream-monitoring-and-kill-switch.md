# Stream Monitoring & Kill-Switch Implementation Plan

> **Status: IMPLEMENTED (with a minor operator-config follow-up open)** on branch `feat/sauron-async-enforcer`. All three phases shipped — Phase 1 (authoritative server-side monitoring), Phase 2 (revocation kill switch + edge/native/jellycompat enforcement), and Phase 3 (async over-cap enforcer). The one still-open item is operator ergonomics, not a phase: the `auth.stream_revocation_poll` / `auth.stream_revocation_ttl` keys are not yet wired through the settings pipeline (defaults 60s / 24h are hardcoded) — see "As-built deltas" and the coverage matrix's follow-up list. A durable Postgres mirror of the kill list was added **after** this plan was written, so kills now survive a server restart or a Redis flush (this plan's "optional Postgres durability" is no longer optional — it closes the gap where restart-resilient playback could reconstruct and re-serve an already-killed stream). For the **as-built** coverage across server layout × playback type × route, the resolved gap findings (GAP-1..GAP-4), and the open verification items, see the companion reference: [`playback-paths-monitoring-kill-matrix.md`](../../architecture/playback-paths-monitoring-kill-matrix.md). The task checkboxes below are retained as the original plan of record.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **Do not edit this plan file while implementing it.**

**Goal:** Gain control over stream abuse (concurrency-cap violations, admin termination, Stremio-style re-streaming) **without** putting the central database on the per-segment hot path and **without** changing the client-facing token protocol (the later-added `Origin` route claim is server-signed and opaque to clients — they only echo the token, so the wire protocol clients see is unchanged). Do it with two primitives plus an off-hot-path decision loop: (1) authoritative server-side monitoring that never trusts the client, and (2) a kill switch that stops any stream within ~120s and keeps it dead.

**Architecture:** Split the problem into *detection* and *enforcement*, and keep both off the hot path.

- **Detection = authoritative monitoring (no client trust).** Bytes can only reach a viewer by flowing through the edge (segment serving) or a held-open direct-play socket that the edge itself is writing. So the server *inherently* knows every live stream — it is the one sending the bytes. We make the existing `nodesessions` records the source of truth, refreshed by *real serve activity* (segment served / active transport / open connection), enriched with `last_served_at` and bytes, and mirror the same signal into integrated mode. No client progress report is trusted for liveness. A stream that "plays quietly" is still pulling segments (or holding a socket) and is therefore visible; one that stops pulling ages out on its own.
- **Enforcement = a revocation kill switch.** A small revocation set lives in Redis (multi-node) or in-memory (integrated), mirrored to Postgres for durability (shipped as a wired part of the kill path, not an optional add-on — it is what lets a kill survive a restart/Redis flush). Edges cache it in memory, refreshed by pub/sub push (near-instant) plus a ≤60s poll (safety net). The **only** hot-path addition is a local in-memory set-membership check per segment — no per-segment Redis or Postgres. On a hit the edge refuses the request and cuts the connection; for direct-play the edge periodically re-checks during the long GET and aborts. A kill "stays dead" because the revocation entry outlives the token and every reconnect is refused.
- **Decision = an async brain, fully off the hot path.** A central background evaluator reads the live monitoring picture and the per-user limits (`users.max_streams`), and decides what to kill: over-cap victims, admin-terminated sessions, and (pluggable) abuse heuristics. Every reason — exceeded limit, admin terminate, Stremio abuse — collapses to the *same* action: write a revocation. Enforcement follows within ~120s. The delay is acceptable for a media server.

The client-facing token **wire protocol is unchanged**. Its `sid`/`uid`/`pid` claims are the revocation keys; the server may add opaque server-signed claims (e.g. the later-added `Origin` route claim) that clients only echo. Nothing about mint sites, segment URLs, HLS manifests, or client behavior changes. This is why the approach is low-risk relative to a short-ticket redesign (see "Background — why not short tickets").

**Tech Stack:** Go, chi routers (proxy/transcode edge + native api), `redis/go-redis/v9`, Postgres via pgx + Goose migrations, the existing `nodeconfig.Watcher` settings pipeline, the `cache` package Redis client + `EventBus` pub/sub.

---

## Commands

Commands assume the repository root is the cwd.

- Build: `make build`
- Backend only: `go build ./... && go vet ./...`
- Lint: `make lint`
- New migration: `make migrate-create NAME=revoked_stream_sessions`
- Local services: `docker compose up -d postgres redis`

---

## Background — what we learned (why this shape)

Audited integration points this plan builds on (do not re-derive):

- **Stream token** is a stateless signed JWT, 24h TTL, verified at the edge by signature only, no store, no revocation today (`internal/streamtoken/token.go`; mints at `internal/api/handlers/playback.go:517,1601,2159` and `internal/jellycompat/handlers_playback.go:317`). Claims carry `sid` (`SessionID`), `uid`, `pid`, `mfid` — our revocation keys.
- **Edge already sees every segment and already writes Redis session records.** Proxy `touchTranscodeSession` → `nodesessions.Tracker.Touch` on every manifest/segment (`internal/proxy/server.go:192`; `internal/nodesessions/tracker.go:137`), 60s TTL, refreshed every 30s. Direct-play/remux are tracked for the request lifetime (`proxy/server.go:143-145,156-158`). Records already carry `AuthUserID`/`ProfileID`/`MediaFileID` (`tracker.go:41-43`) but **no** `last_served_at` or bytes yet.
- **Edge egress is already metered** but only as a node aggregate (`internal/proxy/egress.go`), not per session.
- **Edge Redis is mandatory** and constructed once (`cmd/silo/main.go:576-580,605`), reused for the recipe store (`main.go:623`) — reuse the same client for a revocation store via a `SetRevocationStore(...)` wired exactly like `SetRecipeStore`.
- **Central Redis** is `apiRedisClient` (`main.go:672`, may be nil on Redis-less single-node), injected as `deps.RedisClient` (`internal/api/router.go:135`). Central and edges point at the **same** Redis in multi-node (existing `silo:sessions:*` write/read contract proves it).
- **Config auto-propagates to edges** via `nodeconfig.Watcher` reading `server_settings` (`internal/nodeconfig/watcher.go`); follow the `jellyfin_compat.session_ttl` precedent (`internal/config/config.go:217,233`; `internal/config/db_loader.go:381`) for any new key.
- **Pub/sub already exists** on `cache.ChannelAdmin` and the watcher subscribes to it (`watcher.go:83-93`) — reuse this channel/pattern to push revocations to edges near-instantly.
- **Revocation hook exists but is compat-only.** `OnUserSessionsRevoked` (`router.go:162`, `admin.go:101`, impl `cmd/silo/main.go:2047`) currently only deletes Jellyfin-compat sessions. Admin terminate (`internal/api/handlers/admin_playback_control.go:63` `HandleTerminateSession` → `handleSessionCommand`) currently only sends a realtime command + `stopPlaybackSessionByID` fallback; it does **not** revoke the stream credential at the edge. These are the exact hooks to extend.
- **Concurrency counting already exists in-process** (`internal/playback/session.go` `SessionManager`, per-user `activeCountLocked`, `SessionLimitProvider` reading `users.max_streams`) but only for the local API process and only at admission. The async brain reuses the *limit provider* but computes the live count from the monitoring picture instead of the in-process map, so it works across nodes.

**Why not short tickets (the rejected alternative):** turning the 24h token into a short-lived play ticket would enforce the cap continuously, but (a) Silo's `master.m3u8` is a **VOD** playlist fetched once (`internal/playback/transcode.go:1231` `GenerateFullManifest`), so short tickets embedded in segment URLs would expire mid-movie and 401 the rest of the stream — the "renewal rides on playlist refresh" premise does not hold; (b) flipping the global token TTL is a break-all-playback change that cannot be behaviorally verified without real clients. The monitor-and-kill approach accepts a ~120s enforcement delay in exchange for zero client-protocol change and zero hot-path DB/Redis.

---

## File Structure

### Phase 1 — Authoritative monitoring (no client trust)

- Modify `internal/nodesessions/tracker.go`
  - Add `LastServedAt string` and `BytesServed int64` to `SessionInfo`.
  - Refresh `LastServedAt` (and accumulate `BytesServed`) on real serve activity, not just first touch. Keep the Redis write throttled (only re-`SET` when the record materially changes or on the 30s refresh) so this stays cheap.
  - Add a `Snapshot(ctx) ([]SessionInfo, error)` helper (edge-local view) if useful for `/status`.
- Modify `internal/proxy/server.go`
  - Feed real bytes-served per session into the tracker (wrap the segment/direct/remux response writers, or reuse the `egressMeter` write hook keyed by `claims.SessionID`).
  - Ensure direct-play/remux held-open connections keep `LastServedAt` fresh periodically (a ticking update while `io.Copy`/`ServeFile` runs), so a long quiet pour is never invisible.
- Modify `internal/proxy/egress.go`
  - Allow the metered writer to report per-session deltas to a callback (so the tracker can attribute bytes), in addition to the existing node-aggregate rate.
- Modify `internal/transcodenode/server.go`
  - Mirror the same `last_served_at`/bytes tracking on the node's own segment serve path (`handleSegment`).
- Modify `internal/api/handlers/stream.go` and `internal/api/handlers/playback.go`
  - Integrated mode: on the native segment/direct serve paths, record server-side serve activity (last-served/bytes) into the same monitoring surface (in-memory for single-node; the tracker if Redis present). Stop relying on client progress reports for liveness. Reuse `BeginTransport`/`EndTransport` (`internal/playback/session.go:577,593`) plus a serve-activity touch.
- Modify `internal/api/handlers/nodes.go`
  - Extend `HandleListSessions` (`:303`) to surface `last_served_at`/bytes so the admin view and the brain see the authoritative picture.
- Create `internal/streammonitor/monitor.go`
  - A thin central-side aggregator that returns a normalized "live streams" snapshot: read Redis `silo:sessions:*` (multi-node) or the in-process `SessionManager`/tracker (integrated), keyed by `uid`, with `sid`, method, `last_served_at`, bytes, node. This is the single input to the async brain and to admin monitoring.
- Create `internal/streammonitor/monitor_test.go`
  - Table tests over synthetic session records: liveness aging, per-user grouping, multi-node de-dup by `sid`.

### Phase 2 — Kill switch (revocation) + enforcement

- Create `internal/streamrevoke/store.go`
  - Interface `Store` with `Revoke(ctx, key RevKey, reason string, until time.Time) error`, `IsRevoked(key RevKey) bool` (local, hot-path, in-memory), `List(ctx) ([]Revocation, error)`, `StartSync(ctx)`.
  - `RevKey` addresses a session (`sid`) or a user (`uid`); a user revocation matches all that user's sessions.
  - In-memory backend (integrated): a `map`/set behind an RWMutex with TTL entries; `Revoke` writes locally.
  - Redis backend (multi-node): `silo:revoked:sess:{sid}` / `silo:revoked:user:{uid}` keys with TTL = token remaining life (default 24h); `StartSync` warms an in-memory cache from a `SCAN` on start, subscribes to `cache.ChannelAdmin` for push updates, and polls every ≤60s as a safety net. `IsRevoked` reads only the in-memory cache (no per-call Redis).
  - Postgres durability mirror (see migration), wired central-side, so kills survive a Redis flush/restart; repopulate Redis + cache on startup.
- Create `internal/streamrevoke/store_test.go`
  - Cover: local cache hit/miss, TTL expiry, user-key matches session, pub/sub push applies, poll reconciles, durability repopulation.
- Create `migrations/sql/<timestamp>_revoked_stream_sessions.sql` (via `make migrate-create NAME=revoked_stream_sessions`)
  - Table `revoked_stream_sessions(kind text, key text, reason text, revoked_at timestamptz, expires_at timestamptz, primary key(kind,key))` for durable kills; index on `expires_at` for pruning.
- Modify `cmd/silo/main.go`
  - Construct the revocation store on both sides: edge (reuse `redisClient` from `main.go:576`, wire `srv.SetRevocationStore(...)` next to `SetRecipeStore` at `:623` and the proxy equivalent); central (reuse `apiRedisClient` `:672`, fall back to in-memory when nil). Call `StartSync` for edges.
  - Extend the `OnUserSessionsRevoked` closure (`:2047`) to also `Revoke(user:{uid})` in the store, so account-level revocation kills streams too.
- Modify `internal/proxy/server.go`
  - In `verifyToken` (`:126`) / each serve handler: after signature verify, `if revStore.IsRevoked(sid or uid) { close + 403 }`. For direct-play/remux, run a periodic re-check during the long serve (a deadline/ticker goroutine bound to the request context) that aborts `io.Copy`/`ServeFile` and closes the connection when revoked.
- Modify `internal/transcodenode/server.go`
  - Same revocation check on `handleSegment`/`handleManifest`; refuse + stop feeding a revoked session (and tear down its ffmpeg via the existing session teardown, but ALSO keep the revocation so `reconstructFromToken` refuses to rebuild it — otherwise the node self-reconstructs the kill away, `server.go:620`).
- Modify `internal/api/handlers/admin_playback_control.go`
  - In `handleSessionCommand` for `terminate` (`:63`): in addition to the realtime command + `stopPlaybackSessionByID`, call `revStore.Revoke(sess:{sid})` so termination sticks at the edge regardless of client cooperation. Add an explicit `stop`-vs-`terminate` semantic: `terminate` revokes; `stop` remains a cooperative request.
- Modify `internal/api/router.go`
  - Inject the revocation store into the handlers that need it (`Dependencies` field, like `RedisClient`).
- Modify `internal/config/config.go`, `internal/config/db_loader.go`, `internal/config/yaml_import.go`
  - Add `auth.stream_revocation_poll` (default `60s`) and `auth.stream_revocation_ttl` (default `24h`) following the `jellyfin_compat.session_ttl` precedent. Decide restart-required vs hot-reload (`internal/config/restart_keys.go`); prefer hot-reload.

### Phase 3 — Async decision engine (the brain)

- Create `internal/playback/enforcer.go` (or `internal/worker/stream_enforcer.go`, following the `internal/worker/cleanup.go` ticker precedent)
  - A background loop (central only) that every N seconds: pulls the live snapshot from `streammonitor`, groups by `uid`, reads each user's limit via the existing `SessionLimitProvider` / `users.max_streams`, and for any user over cap selects victims (default: most-recently-started beyond the cap) and calls `revStore.Revoke(sess:{sid}, "over_cap")`.
  - Pluggable rule interface `Rule(snapshot) []KillDecision` so admin/abuse rules compose. Ship two rules: `OverCapRule` and a stub `RestreamHeuristicRule` (documented, disabled by default) that flags sustained N-distinct-title concurrency / throughput anomalies per `uid`.
  - Structured logging + metrics for every kill: who, why, which rule.
- Create `internal/playback/enforcer_test.go`
  - Deterministic tests: over-cap victim selection, no-op when under cap, idempotent re-revocation, limit-provider error → fail-open (do not kill).
- Modify `cmd/silo/main.go`
  - Start the enforcer loop on central (integrated/api), wire it to `streammonitor` + the limit provider + `revStore`.
- Modify `internal/api/handlers/admin_playback_control.go` / a new admin endpoint
  - Optional: expose the current kill decisions / recently revoked sessions for the admin UI ("why was this killed").

---

## Implementation Tasks

### Phase 1 — Authoritative monitoring

- [ ] Add `LastServedAt` + `BytesServed` to `nodesessions.SessionInfo`; refresh on real serve activity with a throttled Redis write.
- [ ] Wrap edge serve writers (proxy segment/direct/remux) to attribute per-session bytes + keep `LastServedAt` fresh during long pours; extend `egressMeter` with a per-session delta callback.
- [ ] Mirror serve-activity tracking on the transcode node (`handleSegment`).
- [ ] Integrated mode: record server-side serve activity on native serve paths; stop trusting client progress for liveness.
- [ ] Add `internal/streammonitor` aggregator returning the normalized per-`uid` live snapshot (Redis and in-process backends) + tests.
- [ ] Surface `last_served_at`/bytes in `HandleListSessions` for the admin view.
- [ ] `go build ./... && go vet ./...`; manual check that a playing stream appears with a moving `last_served_at` and disappears within the TTL after it stops — with the client sending no progress reports.

### Phase 2 — Kill switch

- [ ] Create `internal/streamrevoke` store: interface + in-memory backend + Redis backend (SCAN warm-up, pub/sub push, ≤60s poll, in-memory hot-path cache) + tests.
- [ ] Add durable `revoked_stream_sessions` migration and the Postgres mirror + startup repopulation.
- [ ] Wire the store into edges (`SetRevocationStore` on proxy + transcode Server, `StartSync`) and central (reuse `apiRedisClient`, in-memory fallback).
- [ ] Enforce on the edge hot path: local `IsRevoked` check per segment (refuse + close); periodic re-check + abort for direct-play/remux long pours.
- [ ] Ensure the transcode node does NOT self-reconstruct a revoked session (`reconstructFromToken` consults the store).
- [ ] Extend `OnUserSessionsRevoked` to revoke `user:{uid}`; make admin `terminate` revoke `sess:{sid}` (keep `stop` cooperative).
- [ ] Add config keys (`auth.stream_revocation_poll`, `auth.stream_revocation_ttl`) via the settings pipeline.
- [ ] `go build ./... && go vet ./...`; manual check: revoke a live session → it stops within ~one poll interval and a reconnect with the same token is refused (stays dead); revoke a user → all their streams stop.

### Phase 3 — Async brain

- [ ] Create the enforcer loop + pluggable `Rule` interface; implement `OverCapRule` + stub `RestreamHeuristicRule`; fail-open on limit-provider errors.
- [ ] Start the enforcer on central; wire snapshot + limit provider + revocation store.
- [ ] Structured logging/metrics for every kill; optional admin "recent kills" endpoint.
- [ ] `enforcer_test.go` deterministic tests.
- [ ] `go build ./... && go vet ./...`; manual check: start N+1 concurrent streams for a capped user → within ~120s the over-cap stream(s) are killed and stay dead; under-cap users are untouched.

---

## Testing

- Unit: `internal/streammonitor` (aggregation/aging), `internal/streamrevoke` (cache/TTL/pub-sub/poll/durability), `internal/playback` enforcer (victim selection, fail-open).
- Integration (manual, needs `docker compose up -d postgres redis` + a real client): verify authoritative monitoring with a client that sends no progress; verify kill-within-poll-interval + stays-dead for transcode, direct-play, and remux; verify multi-node (central issues kill, edge enforces) via the shared Redis.
- Regression: existing playback is unaffected when nothing is revoked (the only hot-path addition is a local set lookup that returns false).

## Risks / follow-ups

- **Enforcement latency (~≤120s)** is intentional and accepted; document it so operators expect a short window before a kill takes effect.
- **Direct-play long GET** requires a periodic in-serve revocation check + connection cut; a naive single-GET client with no resume may see an error rather than a clean stop — acceptable for a killed (abusive/over-cap) stream.
- **Redis flush/restart** would drop in-flight revocations unless the Postgres mirror is in place; keep the mirror for durability (reliability-first).
- **Integrated single-node without Redis** uses the in-memory revocation store and in-process monitoring; the durable mirror still applies. No cross-node concern there.
- **Abuse heuristics** (`RestreamHeuristicRule`) ship disabled; tune against real traffic before enabling to avoid false kills.
- **Bytes attribution** on the edge is best-effort for monitoring/heuristics; it must never gate the hot path.

## As-built deltas from this plan

The shipped implementation follows this plan; a few names/shapes differ (the coverage matrix is authoritative for the current state):

- The durable mirror landed as table `stream_revocations(kind, id, reason, revoked_at, expires_at)` (PK `(kind,id)`) via `internal/streamrevoke/durable_postgres.go`, and is **wired central-side and non-optional** — warmed on boot (bounded retry) and re-armed into Redis on the poll tick so multi-node edges reconverge after a Redis flush.
- Enforcement is centralized in two shared helpers `streamrevoke.Store.Refuse` (403) and `WatchAndCut` (in-flight connection cut) used by every serve surface, rather than per-handler ad-hoc checks.
- **Expiry is monotonic on every write.** `applyLocal` and the durable `Upsert` keep whichever copy expires later (`GREATEST`), so the async enforcer's short 5m self-heal TTL can never shorten a longer admin `RevokeSession` on the same session key (which would otherwise reopen the restart-resurrection window the durable mirror closes).
- **In-flight cut required a middleware fix.** `WatchAndCut` uses `http.NewResponseController(w).SetWriteDeadline`, which walks `Unwrap()`. The native and jellycompat middleware `ResponseWriter` wrappers lacked `Unwrap()`, so the integrated cut was a guaranteed no-op until `Unwrap()` was added to `statusWriter`, `requestStatusWriter`, `loggingResponseWriter`, `compatImageProxyTagResponseWriter`, and `debugResponseWriter`.
- **Ownership carry-forward in the monitor.** `streammonitor.mergeStreams` recovers a resolved `UserID`/`ProfileID`/`MediaFileID` (and backfills route/client) across the same-session dedupe, so the transcode node's ownerless start record can't bucket a stream under user 0 and exempt it from the cap.
- **Existence vs timing, made explicit.** Monitoring separates *existence* (server-observed on every path — the transport-count shield integrated, byte-observed at the edge — so a hidden stream that withholds progress is still counted) from *timing* (client progress is trusted, native fully). The transcode serve paths (native `playback.go` and compat `HandleHLSSegment`) gained a per-segment transport marker so a slow single-segment drain can't be reaped mid-serve and the compat HLS path no longer depends on the client's progress POST for liveness. A buffer-ahead idle gap >45s is a remaining reaper follow-up (VERIFY-4).
- **First-class monitoring fields.** Added a `Route` (native/jellycompat) dimension — seeded from `Session.Origin` and carried to edges via a token `Origin` claim — plus client IP/name and position, all surfaced through `SessionInfo`/`LiveStream`. The transcode node's start record is now owner+route+client attributed via `TranscodeStartRequest`. The admin session list unions the in-process integrated sessions (`LiveLocalSessions`) so single-node deployments aren't Redis-blind. Downloads remain an explicit cap exemption (separate quota).
- `auth.stream_revocation_poll` / `auth.stream_revocation_ttl` operator config keys are not yet wired (defaults 60s / 24h are hardcoded) — remaining follow-up.

## AI-use disclosure

This plan was drafted with AI assistance (Claude), based on a read-only audit of the current codebase. No behavior was changed by writing it. The Status banner and As-built deltas section were added after implementation.
