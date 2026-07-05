# Playback Paths — Monitoring & Kill-Switch Coverage Matrix

> Reference checklist for stream monitoring + revocation kill-switch coverage.
> Check every change to playback/monitoring/revocation against this. As-built
> companion to the design plan
> [`2026-07-04-stream-monitoring-and-kill-switch.md`](../superpowers/plans/2026-07-04-stream-monitoring-and-kill-switch.md)
> (that doc is the intent; this one is the shipped coverage). Status as of
> 2026-07-05: monitoring, enforcement, and the async over-cap enforcer are all
> shipped across two code commits — the *monitoring* commit (`feat(playback):
> server-observed stream monitoring`) and the *kill-switch* commit
> (`feat(playback): stream kill switch + async over-cap enforcer`). GAP-1..GAP-4
> are all RESOLVED (see Findings): GAP-4 (kill list did not survive a restart) was
> closed by wiring a durable Postgres mirror, and GAP-3 (in-flight cut for
> integrated direct-play) was closed by adding `Unwrap()` to the native/compat
> middleware writers so the cut reaches the socket. A post-review hardening pass
> then closed GAP-5..GAP-9 found by adversarial re-review: user-kill cutoff
> semantics, compat manifest/subtitle/mint-bypass coverage, terminate-by-id,
> in-flight download cut, admin-list dedupe, and the metered-writer sendfile chain
> (see Findings). That hardening is folded into the two code commits above — the
> branch is organized as monitoring → kill-switch → this docs commit, not as a
> running series of fix commits. (This doc references sibling commits by role, not
> SHA — squash/rebase rewrites hashes, so a SHA citation goes stale the moment the
> branch is amended.)

## Dimensions

- **Server layouts:** (A) integrated, no Redis · (B) integrated, with Redis ·
  (C) multi-node, with Redis (proxy/transcode edge nodes).
- **Playback types:** direct play · remux · transcode (HLS).
- **Routes:** native silo `/api/v1` · jellycompat (`:8096`).

## Key runtime facts (why cells collapse)

- **Integrated (A, B) serves bytes locally.** Node selection returns an empty
  plan when no edge nodes exist, and both native and jellycompat fall through to
  in-process serving (`nodepool/planner.go:248`, `jellycompat/handlers_playback.go:306`
  — the `proxyNode == nil` guard in `buildProxyRedirectURL`). A vs B differ only
  in *revocation propagation* (memory-only vs Redis) and *monitor source*
  (FuncSource vs MultiSource) — the **serve + enforcement points are identical**.
  So A ≡ B for this matrix; treated as one column "Integrated".
- **Multi-node (C) serves bytes on edges.** Native playback hands the client a
  proxy-node URL at session start (`playback.go:1575`); jellycompat 302-redirects
  to a proxy node (`streams.go:120`). Edge serving = `internal/proxy` + `internal/transcodenode`.
- **jellycompat shares the SessionManager.** A jellycompat play starts a real
  `playback.SessionManager` session (`streams.go:1127`), so the monitor/enforcer
  see it exactly like native. The compat `PlaybackSessionStore` is separate
  bookkeeping (PlaySessionId → recipe), not liveness/enforcement.
- **Local transcode fallback:** in multi-node, if no transcode node is available,
  jellycompat falls back to LOCAL transcode (`streams.go:277`,
  `LocalTranscodeFallbackAllowed`) — i.e. the "Integrated jellycompat" serve path
  can also occur under layout C.

## The matrix

Legend: ✅ covered · ⚠️ covered with caveat · ❌ gap.

### Serving handler (where the bytes come from)

| Route | Type | Integrated (A/B) | Multi-node (C) |
|---|---|---|---|
| native | direct | `StreamHandler.HandleStream` → `ServeDirectPlay` (`stream.go:141`) | proxy `handleDirectPlay` (`proxy/server.go:171`) |
| native | remux | `HandleStream` → `ServeRemux` (`stream.go:157`) | proxy `handleRemux` |
| native | transcode | `HandleGetTranscodeSegment` local → `ServeFile` (`playback.go:2860`) | proxy → transcode node `handleSegment` |
| jellycompat | direct | `HandleVideoStream` → `ServeDirectPlay` (`streams.go:153`) | 302 → proxy `handleDirectPlay` |
| jellycompat | remux | `HandleVideoStream` → `ServeRemux` (`streams.go:151`) | 302 → proxy `handleRemux` |
| jellycompat | transcode | `HandleHLSSegment` → `ServeFile` (`streams.go:543`) | 302 → proxy → transcode node |

### Monitoring (server-observed existence, reported to central)

The monitoring model separates **existence** (must be server-observed, never
gated by a client report — the hidden-stream defense) from **timing** (client
progress is fine, especially for native). A disguised re-streamer that pulls
bytes but withholds/falsifies progress must still be counted.

| Route | Type | Integrated (A/B) — existence signal | Multi-node (C) — existence signal |
|---|---|---|---|
| native | direct | ✅ transport-count shield for the whole pour (`stream.go:136`) | ✅ edge tracker, byte-observed (`sessionByteWriter`) |
| native | remux | ✅ transport-count shield (`stream.go:146`) | ✅ edge tracker, byte-observed |
| native | transcode | ✅ per-segment transport marker around `ServeFile` (`playback.go:2860`) | ✅ edge tracker (Touch + AddBytes/segment) |
| jellycompat | direct | ✅ transport-count shield (`streams.go:129`) | ✅ edge tracker, owner+route carried |
| jellycompat | remux | ✅ transport-count shield | ✅ edge tracker, owner+route carried |
| jellycompat | transcode | ✅ per-segment transport marker around `ServeFile` (`streams.go:543`) | ✅ edge tracker, owner+route carried |

- **Existence is server-observed on every cell.** Integrated: a session is
  unreapable while `activeTransportCount > 0`, and every byte-serving path now
  holds that marker — direct/remux for the whole pour, transcode for each segment
  serve (the segment markers were added to close a hidden-stream hole where a slow
  single-segment drain past the 45s grace could be reaped mid-serve; the compat
  HLS path previously refreshed liveness *only* from the client's progress POST,
  a direct violation, now also transport-marked). Edge: a Redis record exists for
  the whole connection (direct/remux) or while segments are pulled (transcode),
  advanced only by real bytes. **No path requires a client progress report to stay
  visible or counted.**
- **Timing is secondary and may trust the client.** Native fully trusts client
  progress for position; the integrated `LastServedAt` is mapped from
  `SessionManager.LastActivityAt` (`cmd/silo/main.go` FuncSource → `handlers.LiveLocalSessions`),
  which client progress can advance. This never inflates the **count** (existence
  is the transport shield, not a heartbeat), so the cap can't be bypassed; it only
  affects which of a user's *own* over-cap sessions `selectVictims` trims first.
  The edge advances `LastServedAt` only on real bytes.
- Report-to-central: **C** = async via Redis (edge writes `silo:sessions:*`,
  `streammonitor.RedisSource` reads). **A/B** = in-process SessionManager IS
  central; read via `streammonitor.FuncSource`. MultiSource unions both, deduped
  by session id.
- Owner + attribution: the transcode node's start record now carries owner + route
  + client (threaded via `TranscodeStartRequest`), and `streammonitor.mergeStreams`
  additionally backfills any missing owner/route/client from another record for the
  same session — so an ownerless-but-freshest node record can never bucket a stream
  under user 0 (which the enforcer skips) or drop route/client from the view.

### Monitoring data model (what central sees per stream)

First-class monitoring goal: see **all** active streams with enough to act on.
Captured per live stream (`streammonitor.LiveStream` / `nodesessions.SessionInfo`):

| Field | Source | Notes |
|---|---|---|
| existence, session id | both | primary; server-observed |
| user id, profile id | token claims / Session | primary; owner attribution |
| media file id | claims / Session | primary; human title is a display-time lookup (follow-up) |
| play method (direct/remux/transcode) | `Type` | primary |
| **route (native/jellycompat)** | `Route` — `Session.Origin` (integrated) or token `Origin` claim (edge) | primary; native and jellycompat share the SessionManager and `Type`, so route is a distinct field seeded at the two `WithClientInfo` sites and carried to edges in the token |
| node serving | NodeName/NodeURL | integrated stamps the local host |
| client ip / client name | `Session.ClientIP/ClientName` (integrated) or edge request + `ClientName` claim | secondary; also the natural re-streaming fingerprint |
| position | `Session.Position` | secondary timing |
| bytes served | edge `AddBytes` | edge only; throughput signal |
| hw/sw, resolution, codecs | Session / node record | secondary |

- **Admin visibility:** `HandleListSessions` unions the Redis edge records with the
  in-process integrated sessions (`LiveLocalSessions`), so a single-node integrated
  deployment is no longer blind (previously Redis-only). The union is deduped by
  session id (`streammonitor.DedupeSessionInfos`, mirroring `mergeStreams`) so a
  stream tracked by both the central manager and the edge serving it shows as ONE
  row — matching the enforcer's count. A `node_id` filter targets
  an edge, so integrated sessions appear only in the unfiltered listing.
- **Downloads are an intentional exemption — with one asymmetry.** `/downloads/*`
  (native) and compat `/Items/{id}/Download` serve full media with no
  session/monitor record and do NOT count against the live-stream cap. The native
  route requires a quota-checked download row (`internal/downloads`
  concurrency/period limits); the compat route requires only compat auth — it is
  covered by NO quota today (bringing it under an Infuse-compatible download
  quota is an open follow-up). Both routes now arm the shared `WatchAndCut`, so a
  per-user stream revocation cuts an in-flight download pour, and the same hook
  deletes every compat login so reconnects need re-auth. Documented at the
  handlers.

### Kill switch (revocation enforced on the serve path)

| Route | Type | Integrated (A/B) | Multi-node (C) |
|---|---|---|---|
| native | direct | ✅ `guardRevocationCut` (Refuse + in-flight cut†) | ✅ `verifyToken` + `cutOnRevocation` (in-flight cut) |
| native | remux | ✅ `guardRevocationCut` (Refuse + cut†) | ✅ verifyToken + cutOnRevocation |
| native | transcode | ✅ `guardRevocation` (refused within one segment) | ✅ proxy verifyToken + node `refuseIfRevoked` + reconstruct guard‡ |
| jellycompat | direct | ✅ `Refuse` + in-flight cut† (GAP-1/3 fixed) | ✅ via proxy; ✅ local fallback guarded |
| jellycompat | remux | ✅ `Refuse` + in-flight cut† | ✅ via proxy; ✅ local fallback guarded |
| jellycompat | transcode | ✅ `Refuse` per segment (GAP-1 fixed) | ✅ via proxy/node; ✅ local fallback guarded |

† In-flight cut uses `streamrevoke.Store.WatchAndCut` (SetWriteDeadline, checked on
entry then every 5s). Works at the edge (the proxy's metered writer implements
`Unwrap`) **and** integrated: the native (`statusWriter`, `requestStatusWriter`)
and compat (`loggingResponseWriter`, `compatImageProxyTagResponseWriter`,
`debugResponseWriter`) middleware writers now implement `Unwrap()`, so
`http.NewResponseController` reaches the socket instead of no-oping (see GAP-3). A
live-client smoke test is still worthwhile, but the previously-guaranteed
integrated no-op is fixed; worst case it still degrades to a next-request `Refuse`.

‡ The transcode node's serve-path check is session-only (`refuseIfRevoked` passes
`userID = 0`), so a per-*user* kill is enforced by the fronting proxy's `verifyToken`
(which passes `claims.UserID`), not at the node's segment serve. The node's
reconstruct guard *does* use the real `claims.UserID`, so a rebuild-after-restart of
a user-killed session is blocked.

### Restart durability (kill survives a process restart / Redis loss)

This axis exists because the branch sits on top of PR #174 (restart-resilient
playback): a session killed before a restart is **reconstructed** afterward from a
durable recipe card, so if the kill list does not also survive the restart the
reconstructed stream is silently re-served. The kill list must be at least as
durable as the thing it kills.

| Deployment | Session survives restart (PR #174) | Kill survives restart | Kill survives Redis flush |
|---|---|---|---|
| A — integrated, no Redis | ✅ recipe card (PG) | ✅ durable PG mirror | ✅ (never used Redis) |
| B — integrated, with Redis | ✅ | ✅ durable PG mirror (+ Redis warm) | ✅ durable PG mirror |
| C — multi-node, with Redis | ✅ | ✅ central durable PG mirror; edges re-warm from Redis | ✅ central re-warms edges from PG→Redis on next write/poll |

\* Steady-state guarantee. Transient boot caveat: if the durable warm exhausts its
bounded retry **and** Redis is empty, the kill list is empty until the first poll
tick (≤60s). This fails *open* by design — see the "Warm is async-tolerant" bullet
below.

- **Hot path unchanged.** `IsRevoked` is still a pure in-memory map read. Postgres
  is touched only on write (`Upsert`), on warm/reconcile (`ListActive` at
  `StartSync` and on the poll tick), and on trim (`Prune`) — never per request.
- **Central-side only.** The durable mirror is wired into the integrated/api
  `streamrevoke.Store` (`cmd/silo/main.go`), not the edge/proxy/transcode nodes,
  which have no app DB and enforce via Redis pub/sub + poll.
- **Bounded growth.** Rows are keyed by `(kind, id)`, so the async enforcer
  re-revoking every pass UPSERTs one row rather than accumulating; expired rows are
  physically reclaimed by `Prune` on the poll tick, and `ListActive` filters on
  `expires_at > now()`.
- **Expiry is monotonic.** Both the in-memory `applyLocal` and the durable
  `Upsert` keep whichever copy expires LATER (`GREATEST`), never shortening an
  existing kill. This matters because the async over-cap enforcer re-revokes with a
  short 5m self-heal TTL on the same `KindSession` key an admin may have killed for
  24h; without monotonic expiry the enforcer would silently shrink the admin kill
  to 5m and reopen the restart-resurrection window this whole axis defends against.
- **Warm is async-tolerant.** By product decision, a just-reconstructed revoked
  stream may serve a few seconds before the durable warm/next Refuse tick cuts it;
  no blocking startup ordering is required. Edge caveat: if the boot durable warm
  exhausts its bounded retry **and** Redis is empty, the kill list is empty until
  the first poll tick (≤60s). This fails *open* by design (a kill switch cannot
  fail closed without blocking all playback when the DB is briefly unavailable).
- **Edge Redis-outage fails open.** Edge nodes (proxy/transcode) have no durable
  store and learn *new* kills only from Redis pub/sub + SCAN. While Redis is
  unreachable, already-cached kills persist but a kill issued *during* the outage
  does not reach the edge until Redis recovers — enforcement fails open there.

## Findings

> GAP-1..GAP-3 all shipped in the kill-switch commit (see the Status banner for
> how commits are referenced by role rather than SHA).

**GAP-1 — RESOLVED.** jellycompat LOCAL serving now consults the
shared `Store.Refuse` in `HandleVideoStream` (direct/remux) and `HandleHLSSegment`
(per segment), closing the integrated / local-transcode-fallback hole.

**GAP-2 — RESOLVED.** `buildProxyRedirectURL` now stamps
`uid`/`pid`/`mfid` onto the jellycompat proxy-redirect token (looked up from the
upstream SessionManager session), so edge monitoring/kill attribution matches
native. `AuthUserID = 0` no longer occurs for jellycompat.

**GAP-3 — RESOLVED.** The in-flight cut is shared as
`streamrevoke.Store.WatchAndCut` and applied to native `/stream`
(`guardRevocationCut`) and jellycompat `HandleVideoStream`. The original caveat —
`SetWriteDeadline` might not reach the socket through the native/compat middleware
chain — was **confirmed** as a guaranteed integrated no-op (four middleware writers
wrapped `w` without `Unwrap()`) and then fixed by adding `Unwrap()` to
`statusWriter`, `requestStatusWriter`, `loggingResponseWriter`,
`compatImageProxyTagResponseWriter`, and `debugResponseWriter`. The cut now reaches
the socket in integrated mode; if it ever no-ops again the stream still stops on
its next request via `Refuse` (no regression). See VERIFY-1.

**GAP-4 — RESOLVED.** The kill list did not survive a process restart or a Redis
flush, but PR #174's restart-resilient playback *does* reconstruct the session — so
a stream revoked before a restart was silently re-served afterward. This was
universal in deployment A (integrated, no Redis: the kill list was pure RAM) and
occurred on any Redis loss in B/C. Fixed by wiring a concrete Postgres
`DurableStore` (`internal/streamrevoke/durable_postgres.go`, table
`stream_revocations`) into the central `streamrevoke.Store`: `Revoke` mirrors to
Postgres, `StartSync` warms from it on boot, and the poll tick re-warms (heals a
Redis flush) and `Prune`s expired rows. The `defaultTTL` (24h) is held `>=` the
recipe-card `MaxTokenTTL` (24h) as an invariant so a kill cannot expire before its
session can be reconstructed, and expiry is **monotonic** on every write (in-memory
`applyLocal` + durable `Upsert` `GREATEST`) so the async enforcer's short 5m
re-revoke can never shorten a longer admin kill on the same session key. Hot path
(`IsRevoked`) stays an in-memory read.

**GAP-5 — RESOLVED (post-review).** User-kind revocations were a blanket ban:
any admin user edit (via `OnUserSessionsRevoked`) 403'd that user's playback for
24h even after re-login, unrevocably. Fixed with cutoff semantics — see
"user-ID kills" above.

**GAP-6 — RESOLVED (post-review).** Compat coverage holes: `HandleMasterManifest`
/ `HandleHLSManifest` had no revocation check and kept (or, post-restart,
re-spawned) a killed session's ffmpeg via the ensure path; `HandleVideoStream`
checked revocation only after `ensureUpstreamPlayback`, which can replace an
unreconstructable killed session with a fresh id that passed the check (kill
dodged by re-hitting the URL); `HandleSubtitleStream` ignored kills entirely.
All four now `Refuse` up front.

**GAP-7 — RESOLVED (post-review).** Admin terminate 404'd before revoking when
the session was absent from the central in-memory manager — exactly the
edge-served / post-restart / progress-withholding streams the kill switch
exists for (the admin list showed them via the Redis union; terminate couldn't
touch them). Terminate now writes the revocation FIRST, keyed on the session id
alone, and answers `202 {status: "revoked"}` when there is no local session for
the cooperative command.

**GAP-8 — RESOLVED (post-review).** The admin session list double-counted every
edge-served stream (central manager row + edge Redis row, no dedupe), unlike
the enforcer's merged picture. Now deduped by session id with owner/attribution
carry-forward (`streammonitor.DedupeSessionInfos`).

**GAP-9 — RESOLVED (post-review).** The "restore sendfile" commit was dead code
end-to-end: every stream route runs inside `meterEgress`, and
`meteredResponseWriter` hid `io.ReaderFrom`, so `sessionByteWriter.ReadFrom`'s
fast path never fired and all direct-play/remux bytes went through a userspace
copy. `meteredResponseWriter` now forwards `ReadFrom` (metering the returned
total); a chain test locks the full production writer stack onto the sendfile
path. Related tracker fixes in the same pass: `Touch`/`Track` preserve the
first-seen `StartedAt` (was reset per segment/range request, corrupting the
enforcer's tie-break and the admin start time), `AddBytes` no longer recreates
entries for pruned sessions (slow permanent leak), the proxy→node segment pour
attributes bytes incrementally (a slow drain can't go invisible mid-segment or
post bytes to a dead record), and the transcode node marks serve activity so
its record's `LastServedAt` is no longer frozen at start time.

**GAP-3 (original, superseded) — no in-flight cut for native/compat LOCAL direct-play.**
The original finding: `guardRevocation` refused the next request/reconnect but could
not hang up an in-flight `ServeFile` on the central process for a single long GET.
Now closed — see GAP-3 above (shared `WatchAndCut` + middleware `Unwrap()`). Even
if the cut degrades, admin terminate also fires the realtime command + session stop,
so a killed direct-play stops on its next request at worst.

## Open verification items (check before prod)

- [x] **VERIFY-1 — in-flight cut reaches the socket on native + jellycompat — RESOLVED.**
  `streamrevoke.Store.WatchAndCut` cuts a revoked long-GET direct-play/remux by
  setting a zero write deadline via `http.NewResponseController(w)`, which walks the
  writer chain via `Unwrap()`. This was **confirmed broken** in integrated mode:
  `statusWriter`/`requestStatusWriter` (native) and `loggingResponseWriter`/
  `compatImageProxyTagResponseWriter`/`debugResponseWriter` (compat) wrapped `w`
  without `Unwrap()`, so `SetWriteDeadline` returned `ErrNotSupported` and the cut
  no-oped. Fixed by adding `Unwrap() http.ResponseWriter` to all five (pattern from
  `activitylog/middleware.go:186`). A live smoke test is still worthwhile: start a
  native and a jellycompat single-long-GET direct-play, admin-terminate it, confirm
  the connection drops within ~5s rather than only on the next request.
- [x] **VERIFY-2 — download routes vs the stream cap — RESOLVED (documented exemption).**
  `/downloads/*` (native) and compat `/Items/{id}/Download` serve full media with no
  session/monitor record; they are intentionally exempt from the live-stream cap and
  governed by the separate download concurrency/period quota. Documented at both
  handlers and in the monitoring data model above. (Note: they also do not consult
  the kill switch — a per-user *stream* revocation does not stop an in-flight
  download. If a ban should also cut downloads, guard those handlers separately.)
- [ ] **VERIFY-4 — transcode buffer-ahead evasion (HOLE 2).** A transcode client
  (native or compat) that buffers far ahead and pauses segment fetches for >45s (the
  active grace) is transiently reaped, then self-heals on the next fetch — so a
  determined abuser could time buffer-pauses to duck under the cap at the enforcer's
  tick. The per-segment transport marker fixes the mid-serve case but not the idle
  gap between segments. Fix needs a reaper change: treat a running transcode ffmpeg
  as liveness, or a transcode-specific grace ≥ the client's max buffer. Deferred
  because it touches the sensitive session-reaping path (cf. #279).
- [ ] **VERIFY-3 — multi-replica central enforcement.** The async enforcer runs on
  every integrated/api process. Two *integrated* replicas behind a load balancer
  each see only their own in-process `FuncSource` (integrated mode writes no
  `silo:sessions:*` records), so a user split across both can exceed `max_streams`
  undetected; multiple *api* replicas each run an independent enforcer and revoke
  the same victims (idempotent, but redundant write load). A single-leader election
  for the enforcer, or having integrated nodes publish their local sessions to the
  shared Redis picture, would close this.

## Design assessment (shared vs duplicated code)

**Kill DECISION is well-centralized — keep it.** All revocation state and the
`IsRevoked(sessionID, userID)` check live in one place (`internal/streamrevoke`).
Session kills and user kills use the SAME store and the SAME check — `RevokeSession`
and `RevokeUser` write into one `items` map; `IsRevoked` tests both keys. No
duplication at the decision layer.

**Kill ENFORCEMENT — DONE.** The "extract sid/uid → refuse (403)"
step is now a single shared method `streamrevoke.Store.Refuse(w, sessionID, userID)
bool`. All four serve surfaces call it (proxy `verifyToken`, transcode node
`refuseIfRevoked`, native `guardRevocation`, jellycompat serve handlers); each only
owns its token extraction (path `{token}`, query `st`, or compat `PlaySessionId`).
The in-flight connection-cut is the shared `WatchAndCut` (SetWriteDeadline), used by
both the edge (`cutOnRevocation`) and the native/compat long-pour paths. ONE section
per concern, as intended.

**Monitoring WRITE side is two implementations by necessity — leave it.** Edges
have no SessionManager; the integrated process has no tracker. `nodesessions.Tracker`
(edge) and `SessionManager` (integrated) are different runtimes, not duplicated
logic, and they already converge at the single read layer (`streammonitor`,
unioned by MultiSource). Forcing one implementation would be worse.

**user-ID kills — plumbing check (as asked).** Two distinct layers, correctly
separate:
- *Auth/login revocation* (pre-existing): `OnUserSessionsRevoked` revokes native
  auth sessions (`auth.SessionRepository.RevokeAllByUser`) and drops compat login
  tokens (`SessionStore.DeleteByUserID`). This governs "can this user authenticate",
  not "stop this byte stream".
- *Stream revocation* (new): `streamRevocation.RevokeUser` writes a `KindUser`
  entry into the SAME `streamrevoke` store as session kills. Session-level and
  user-level STREAM kills already share all plumbing (one store, one `IsRevoked`).
- **A user kill is a CUTOFF, not a ban (GAP-5 fix).** `IsRevoked` matches a
  `KindUser` entry only when the stream's credential predates the revocation:
  the stream token's `iat` on token-bearing surfaces (edge proxy, transcode
  node, native `?st=`), the request entry time on freshly-authenticated
  surfaces (native session auth, compat login — every pre-revocation login is
  reset by the same hook, so reaching a serve path afterward proves fresh
  auth). An in-flight pour always predates a later revocation, so mid-pour
  user kills still cut. An unknown (zero) credential time never matches (fail
  open). This is what makes it safe for `OnUserSessionsRevoked` — which fires
  on ANY admin edit of password/role/enabled/permissions/quality — to also
  write a stream kill: without the cutoff, a routine permission tweak 403'd
  the user's playback for the full 24h TTL after re-login, with no unrevoke
  path (expiry is deliberately monotonic).
- These two layers should NOT be merged — conflating "may log in" with "this
  stream must die" would couple auth and playback. They are chained in the same
  hook, which is the right seam.

## Recommended follow-up order

GAP-1, GAP-2, GAP-3, and GAP-4 are all shipped (see Findings). Already-shipped
hardening not to re-do: `Revoke` propagation uses `context.WithoutCancel`, so an
aborted admin request can no longer strand a kill in central memory only.
Remaining work:

1. **Operator config keys (small):** wire `auth.stream_revocation_poll` /
   `auth.stream_revocation_ttl` through the settings pipeline; defaults (60s / 24h)
   are currently hardcoded in `internal/streamrevoke/store.go`.
2. **Transcode buffer-ahead liveness (VERIFY-4 / HOLE 2):** treat a running ffmpeg
   as liveness (or a transcode-specific grace) so a buffered-ahead transcode isn't
   transiently reaped and can't time buffer-pauses to duck the cap.
3. **Media title enrichment:** resolve `MediaFileID → title` at admin display time
   so the view shows *what* is being watched, not just a numeric id (also enables a
   distinct-title re-streaming heuristic).
4. **Multi-replica enforcement (VERIFY-3):** single-leader election for the async
   enforcer, or have integrated nodes publish local sessions to the shared Redis
   picture so the cap holds across replicas.
5. **Self-heal a failed durable write:** `Revoke` mirrors to Postgres best-effort;
   a transient DB error at revoke time is logged but never repaired (the maintain
   tick only reads *from* durable, never re-mirrors live in-memory kills *to* it),
   so that one kill is lost on the next restart. Add a memory→durable re-mirror on
   the poll tick.
6. **Invariant guard test:** `defaultTTL >= playback.MaxTokenTTL` is coupled only by
   a prose comment (streamrevoke can't import playback — import cycle). Add a test
   in a package that can import both, so raising `MaxTokenTTL` can't silently
   violate it.
7. **Transcode-node ghost records:** the node's session-backed `Track` record is
   refreshed forever until an explicit stop/cleanup; if central dies or loses the
   session without the stop reaching the node, an owner-attributed ghost persists
   in Redis indefinitely — permanently +1 in the user's live count (the enforcer's
   freshness ordering trims the ghost first, so real streams survive, but the
   enforcer re-revokes it every pass and the admin view overcounts). Needs a
   node-side idle sweep tied to real serve activity (`MarkServed` now provides the
   signal).
8. **Compat download quota:** compat `/Items/{id}/Download` is exempt from the
   stream cap AND covered by no download quota (native downloads require a
   quota-checked download row; the compat route requires only compat auth). An
   Infuse-compatible quota (plain GETs, Range requests, no download rows) is open
   product/design work; today's controls are compat auth, the library access
   filter, and the in-flight user-revocation cut.
