# Stream & API Abuse — Coverage Matrix

> **What this is.** A red-team assessment of how the `feat/sauron-async-enforcer`
> branch (server-observed stream monitoring + revocation kill switch + async
> over-cap enforcer) — layered on the current `main` — reacts to ~29 concrete
> abuse stories. Each story is scored on two axes the branch itself separates:
>
> - **Detection** — does the server *see* the abuse and *label it correctly*
>   (right user, right stream, right kind)?
> - **Enforcement** — can the server *stop* it, and *how fast*?
>
> **Companion docs:** the intent lives in
> [`../superpowers/plans/2026-07-04-stream-monitoring-and-kill-switch.md`](../superpowers/plans/2026-07-04-stream-monitoring-and-kill-switch.md);
> the as-built coverage-by-path lives in
> [`playback-paths-monitoring-kill-matrix.md`](playback-paths-monitoring-kill-matrix.md).
> This doc is the *adversarial* companion: it assumes a motivated abuser and asks
> where the design ends.
>
> **Scope note.** The branch is scoped to **concurrent playback-stream abuse**. A
> large fraction of real-world "abuse" (library enumeration load, bulk ripping,
> API request floods, CPU exhaustion, re-broadcasting) lives *outside* that scope.
> Where that is the case this doc says so plainly and points at the subsystem that
> would actually own the defense (`internal/ratelimit`, `internal/downloads`,
> catalog browse, node scheduling) — several of which have real gaps of their own.

## Legend

| Symbol | Detection | Enforcement |
|---|---|---|
| ✅ | Seen and correctly attributed | Stopped (and how fast) |
| ⚠️ | Seen but partial / mislabeled / lossy | Partial, slow, best-effort, or config-dependent |
| ❌ | Invisible to the server | Cannot stop |

Enforcement latency budget for the async path is **~120s** = enforcer tick
(`DefaultInterval = 30s`, `streamenforcer/enforcer.go`) + revocation propagation
(pub/sub push immediate; ≤60s poll safety net, `streamrevoke/store.go`) + in-flight
cut (5s `WatchAndCut` ticker for long pours; next segment for HLS). Synchronous
admission refusal (`ErrTooManyStreams`, `playback/session.go`) is **immediate**.

---

## Three corrections the investigation turned up (read first)

The branch docs oversell three things. The stories below are scored against the
**code**, not the plan.

1. **The async enforcer does nothing for a *standard* user.** The enforcer's limit
   comes from the **raw** `users.max_streams` column (`cmd/silo/main.go`, the
   `LimitFunc`), and `limit <= 0` is treated as "unlimited → skip"
   (`streamenforcer/enforcer.go`). Migrations `20260702180000` / `20260702190000`
   set every standard user's `users.max_streams = 0` and delegate the real cap (5)
   to the **Default Group**. So for a normal user the enforcer sees `limit 0` and
   **never revokes**. The cap that actually holds is the *synchronous admission*
   check, which uses the group-merged effective policy
   (`access.EffectivePolicyForUser` → `strictestPositive`). The "async brain" only
   bites users given an explicit per-user `max_streams > 0`. Treat this as a
   **bug**, not a design choice — it means the branch's headline feature is
   effectively dormant on a default install.

2. **The re-streaming heuristic does not exist.** The plan describes shipping
   `OverCapRule` + a disabled `RestreamHeuristicRule` behind a pluggable `Rule`
   interface. In code, `streamenforcer/enforcer.go` is a single hardcoded over-cap
   loop — no rule interface, no restream logic, not even a disabled stub. The
   fingerprints a heuristic *would* need (`ClientIP`, `ClientName`, `MediaFileID`,
   `BytesServed`) are captured in `streammonitor` but nothing consumes them for
   detection.

3. **There is no server-wide or per-node transcode/CPU cap.** Only a per-*user*
   `max_transcodes` (default 2, `<=0` = unlimited) checked at admission. The one
   ffmpeg semaphore (`reconstructSem`, sized to `NumCPU`) guards **only** the
   restart-reconstruct path in `transcodenode/server.go`, **not** fresh starts.

One thing the docs *under*-sell: `main` already ships a real general API rate
limiter (`internal/ratelimit/`), enabled by default — but it is mounted only on
the authenticated native surface + specific auth endpoints, and the entire
Jellyfin-compat surface is unthrottled (see Category E).

---

## Summary matrix

### Category A — Concurrency-cap abuse (the branch's core competency)

| # | Abuse story | Detection | Enforcement | One-line verdict |
|---|---|---|---|---|
| 1 | User opens N+1 concurrent **transcodes** (over cap) | ✅ | ✅ immediate | Admission refuses the N+1 start synchronously |
| 2 | User opens N+1 concurrent **direct-play** streams | ✅ | ✅ immediate | Same admission gate; method-agnostic |
| 3 | **Shared account**, many devices streaming at once | ✅ | ✅ / ⚠️ | Capped per-user on one process; leaks across replicas (#7) |
| 4 | **Buffer-ahead then pause** fetches >45s to duck the cap | ❌ (window) | ❌ (window) | VERIFY-4 hole: transcode goes invisible 45s/60s, admits another |
| 5 | **Withhold/falsify progress** to hide a stream | ✅ | ✅ | Existence is byte-observed; progress only moves a display field |
| 6 | **Rapid session churn** between enforcer ticks | ⚠️ | ⚠️ | Admission blocks over-cap starts; no rate limit on `/start` |
| 7 | Split streams across **multiple integrated replicas** | ⚠️ | ⚠️ | Admission is per-process; enforcer is the only cross-node backstop (and see correction #1) |
| 8 | **Standard user** exceeds cap via the edge/multi-node path | ⚠️ | ⚠️ | Enforcer no-ops (`users.max_streams = 0`); only admission holds |

### Category B — Admin control / kill switch

| # | Abuse story | Detection | Enforcement | One-line verdict |
|---|---|---|---|---|
| 9 | Admin **terminates** a specific abusive live session | ✅ | ✅ | Terminate writes revocation first, keyed on sid; 202 even if no local session |
| 10 | Admin **bans a user** (revoke all their streams) | ✅ | ✅ | `RevokeUser` cutoff kills pre-ban streams incl. in-flight |
| 11 | Killed stream **reconnects** with the same token | ✅ | ✅ | Revocation outlives token; every reconnect `Refuse`d |
| 12 | Killed stream **survives a server restart** | ✅ | ✅ | Durable Postgres mirror re-warms the kill list; monotonic expiry |
| 13 | Kill issued **during an edge Redis outage** | ⚠️ | ⚠️ | Edge fails **open**: new kills don't reach edge until Redis recovers |

### Category C — Re-streaming / redistribution

| # | Abuse story | Detection | Enforcement | One-line verdict |
|---|---|---|---|---|
| 14 | Re-broadcast **one** Silo stream to many external viewers | ❌ | ❌ | Under-cap = untouched; no restream heuristic exists (correction #2) |
| 15 | **Many** concurrent Silo streams feeding a restream service | ⚠️ | ✅ ~120s | Caught only as raw over-count, not labeled re-streaming |
| 16 | **Mint & hoard** many 24h tokens, fan out later | ❌ | ❌ / ⚠️ | No mint cap, signature-only, stop doesn't revoke; caught only if fan-out becomes over-cap |

### Category D — Ripping / bulk data exfiltration

| # | Abuse story | Detection | Enforcement | One-line verdict |
|---|---|---|---|---|
| 17 | Rip whole library via **native download** endpoints | ⚠️ | ⚠️ | 3-concurrent gate only; **no volume cap**; loop create→complete defeats it |
| 18 | Rip via **compat `/Items/{id}/Download`** (Infuse) | ❌ | ❌ | **No quota at all** — auth + library filter only. Widest hole |
| 19 | Rip via **sequential direct-play GETs** (one at a time) | ⚠️ | ❌ | Cap counts concurrency, not volume; stays at 1 forever |
| 20 | Admin **stops an in-progress rip** mid-transfer | — | ⚠️ | Branch adds `WatchAndCut` on downloads (best-effort); admin must issue the revoke |

### Category E — API / DB load abuse (non-stream; branch is orthogonal)

| # | Abuse story | Detection | Enforcement | One-line verdict |
|---|---|---|---|---|
| 21 | **Infuse pulls the whole library list on every app start** | ❌ | ⚠️ | Per-page capped at 1000; full browse is uncached & unthrottled |
| 22 | **Thundering herd** — many clients cold-cache full enumeration | ❌ | ⚠️ | Latest/hub rails singleflight; general browse does not |
| 23 | Client **pages a 100k+ library as fast as possible** | ❌ | ❌ | Per-page COUNT + rising OFFSET; no page-rate limit, no cache |
| 24 | **Expensive sorted/filtered views** over the whole library | ❌ | ⚠️ | Some sorts index-optimized; `DateCreated`/filters = full top-N + COUNT, uncapped |
| 25 | Badly-coded client **retry storm** (thousands req/s) | ✅ | ⚠️ | Native authed surface throttled (per-IP 120rps); compat & `/auth/refresh` not |
| 26 | Tight-loop polling **`/auth/refresh`** | ✅ | ❌ | No rate-limit middleware on that route |
| 27 | **Credential stuffing** via compat `/Users/AuthenticateByName` | ✅ | ❌ | Compat router mounts no limiter; native login is capped 20/min |
| 28 | Many concurrent **HTTP connections / slowloris** | ❌ | ❌ | No connection cap / `LimitListener`; only Read/Write/Idle timeouts |
| 29 | Spawn many concurrent **transcodes to exhaust CPU** | ⚠️ | ❌ | Per-user `max_transcodes` only; no per-node/server-wide ffmpeg cap |

---

## Detailed stories

### Category A — Concurrency-cap abuse

**A1. N+1 concurrent transcodes (over cap).**
The (N+1)th `StartSession` returns `ErrTooManyStreams` *synchronously* at admission
(`playback/session.go`, `inlineAdmissionErrorLocked`), using the group-merged
effective cap (5 for a standard user). The client never gets a session. The async
enforcer is a redundant backstop *only* for users with an explicit
`users.max_streams > 0` (correction #1). **Detection ✅ / Enforcement ✅ (immediate).**

**A2. N+1 concurrent direct-play streams.**
Direct-play sessions are `Track`-backed and counted identically at admission — the
`MaxStreams` check is method-agnostic. Same immediate refusal. **✅ / ✅.**

**A3. Shared account, many devices at once.**
All sessions carry the same `AuthUserID`; `streammonitor.ByUser` groups them and
`Client*` fields expose the fan-out. The per-user cap bounds simultaneous streams
to the effective `MaxStreams`. **Caveat:** the cap is per-process (see A7), so
spreading devices across integrated replicas multiplies the allowance.
**Detection ✅ / Enforcement ✅ on one process, ⚠️ across replicas.**

**A4. Buffer-ahead then pause (VERIFY-4 hole). — REAL, UNFIXED.**
A transcode client that pre-buffers far ahead and stops fetching segments for
>45s (integrated active grace, `session.go`) / >60s (edge `sessionTTL`,
`nodesessions/tracker.go`) stops counting at admission
(`countsTowardLimitsLocked`) and is reaped from the snapshot by `CleanStale` /
idle-prune — while the client keeps playing from buffer. During that window the
server admits *another* stream. Staggered pre-buffer/pause rotation across devices
ducks the instantaneous cap. Direct-play/remux are immune (their `Track` records
are never idle-timed). **Detection ❌ (during window) / Enforcement ❌ (during
window).** Fix requires treating a running ffmpeg as liveness or a transcode-grace
≥ client buffer.

**A5. Withhold/falsify progress reports.**
This *fails* as an evasion. Existence and liveness are **byte-observed**
(`BeginTransport`/`EndTransport` bracket every pour; `AddBytes`/`MarkServed` set
`LastServedAt`), never derived from client progress. Falsifying `Position` only
corrupts a secondary display field and, at worst, changes which of the user's
*own* over-cap sessions `selectVictims` trims first. **Detection ✅ / Enforcement ✅.**

**A6. Rapid session churn between ticks.**
No rate limit exists on `/start` or `/transcode/start` (the `ratelimit`
middleware is wired only to auth endpoints). In integrated mode synchronous
admission still refuses over-cap starts instantly, so churn can't exceed the cap
locally. The gap is the multi-node/edge picture, where admission may have run on a
different node and the enforcer only reconciles every ~120s.
**Detection ⚠️ / Enforcement ⚠️ (integrated fast, edge lags).**

**A7. Split across multiple integrated replicas.**
`activeCountLocked` counts only the local `SessionManager` map — there is no
distributed admission counter. Two integrated replicas each admit up to the cap
independently. The enforcer is the only cross-replica backstop, but (a) integrated
replicas write no `silo:sessions:*` records, so a remote enforcer can't see them,
and (b) per correction #1 it no-ops for standard users anyway.
**Detection ⚠️ / Enforcement ⚠️.** (VERIFY-3.)

**A8. Standard user exceeds the cap on the edge path.**
Direct consequence of correction #1. The enforcer — the component *specifically*
built to enforce the cap across nodes — is dormant for `users.max_streams = 0`.
On a single integrated node admission still holds; in multi-node, an over-cap edge
situation that admission didn't catch locally will **not** be reconciled. **⚠️ / ⚠️.**

### Category B — Admin control / kill switch

**B9. Admin terminates a specific session.**
`terminate` writes the revocation **first**, keyed on the session id alone, then
issues the cooperative realtime command; it answers `202 {status:"revoked"}` even
when the session is absent from the central in-memory manager (GAP-7) — exactly the
edge-served / post-restart / progress-withholding streams the kill switch exists
for. **Detection ✅ / Enforcement ✅.**

**B10. Admin bans a user.**
`RevokeUser` writes a `KindUser` entry; `IsRevoked` matches it for any of the
user's sessions whose credential predates the revocation (cutoff semantics, GAP-5),
so in-flight pours are cut (`WatchAndCut`) and reconnects `Refuse`d, while a
later legitimate re-login is not permanently banned. **✅ / ✅.**

**B11. Killed stream reconnects with the same token.**
The revocation entry outlives the token TTL, so every reconnect with the same
credential is refused. "Stays dead." **✅ / ✅.**

**B12. Killed stream survives a restart.**
The durable Postgres mirror (`streamrevoke/durable_postgres.go`, table
`stream_revocations`) is warmed on boot and re-armed on the poll tick, and expiry
is monotonic (`GREATEST`) so the enforcer's short 5m self-heal TTL can't shorten a
24h admin kill. This closes GAP-4, where PR #174's restart-resilient playback would
otherwise reconstruct and re-serve a killed stream. **✅ / ✅.** *Transient boot
caveat:* if the durable warm exhausts its bounded retry **and** Redis is empty, the
kill list is empty until the first poll tick (≤60s) — fails **open** by design.

**B13. Kill issued during an edge Redis outage.**
Edge nodes have no durable store and learn *new* kills only from Redis pub/sub +
SCAN. While Redis is unreachable, already-cached kills persist but a kill issued
*during* the outage does not reach the edge until Redis recovers. **Detection ⚠️ /
Enforcement ⚠️ (fails open at the edge).**

### Category C — Re-streaming / redistribution

**C14. Re-broadcast one Silo stream to many external viewers. — NO DEFENSE.**
The classic Stremio/Plex-share abuse: one account pulls a single stream and a proxy
fans it out to hundreds. It stays at concurrency 1, so admission and the enforcer
never fire, and the re-streaming heuristic that would catch it *does not exist*
(correction #2). The fingerprints to build it (`ClientIP`, `ClientName`,
`BytesServed`, distinct `MediaFileID`) are captured but unused. **Detection ❌ /
Enforcement ❌.**

**C15. Many concurrent streams feeding a restream service (over cap).**
This *is* caught — but only as a raw over-count, not labeled as re-streaming.
`ByUser` counts sessions per uid; the enforcer revokes victims beyond the limit
within ~120s (and admission refuses new over-cap starts immediately). Relies on
correct owner attribution (jellycompat/edge records must carry the owner or they
bucket under user 0 and are skipped — mitigated by `mergeStreams` owner-adoption).
**Detection ⚠️ (as over-cap, not as restream) / Enforcement ✅ ~120s** — *subject to
correction #1 if the user has no explicit per-user cap.*

**C16. Mint & hoard 24h tokens, fan out later.**
Stream tokens are signature-only 24h JWTs (`streamtoken/token.go`) with no `jti`,
no one-time-use, no per-user mint counter. A normal Stop does **not** write a
revocation, so a token for a stopped session stays cryptographically valid for its
full 24h. Hoarding is free and invisible at mint time. Fan-out is caught *only* if
the aggregate live use trips the per-user over-cap enforcer — i.e. a low-concurrency
fan-out (few tokens, each widely re-streamed) collapses back into C14 and evades
everything. **Detection ❌ (at mint) / Enforcement ❌ (unless it becomes over-cap).**

### Category D — Ripping / bulk data exfiltration

**D17. Rip via native download endpoints.**
Bounded only by `download.max_concurrent_per_user` (default **3**,
`internal/downloads/limiter.go`). `download.max_per_period` and both bandwidth
knobs default to **0 = unlimited** — and the bandwidth managers are *rate* throttles
(token buckets), never *volume* caps. The concurrency check runs at record
creation, so a `create → complete → create` loop pulls the entire library 3 files
at a time forever. **No total-volume cap exists anywhere.** **Detection ⚠️
(per-download rows, no cumulative signal) / Enforcement ⚠️ (weak concurrency gate).**

**D18. Rip via compat `/Items/{id}/Download`. — WIDEST HOLE.**
`HandleDownload` (`jellycompat/streams.go`) requires only a compat session and the
per-item library-access filter, then serves the raw file with full Range support.
**No download row, no quota, no bandwidth throttle, no session/monitor record, no
byte cap.** An Infuse-style client can GET/Range every original file back-to-back,
unlimited, and the server keeps no record beyond generic HTTP logs. The branch's own
doc comment admits this and files it as an open follow-up. **Detection ❌ /
Enforcement ❌.**

**D19. Rip via sequential direct-play GETs.**
Playing items one at a time keeps `activeCountLocked == 1`, always under the cap.
Because the cap counts concurrency and never cumulative bytes, sequential
direct-play of every raw file is a fully-permitted rip. Each play *is* an
attributable session, but nothing correlates sequential sessions into "this user is
ripping." **Detection ⚠️ / Enforcement ❌.**

**D20. Admin stops an in-progress rip mid-transfer.**
On `main`, impossible — `Refuse` only 403s *new* requests; an open multi-GB GET
keeps pouring. The branch adds `streamrevoke.Store.WatchAndCut` (checks on entry +
every 5s, forces the socket via `SetWriteDeadline`) and arms it on native downloads
(`api/handlers/downloads.go`) and compat download/direct-play
(`jellycompat/streams.go`). So a **user revocation now cuts an in-flight download**
— best-effort (no-op if the writer chain lacks write-deadline support, then stops on
next request). The admin still has to *decide* to revoke; nothing auto-detects the
rip. **Enforcement ⚠️ (branch improves `main`'s ❌ to best-effort cut).**

### Category E — API / DB load abuse (branch is orthogonal)

**E21. Infuse pulls the whole library list on every app start.**
Per-page size is hard-capped (`compatBrowseMaxLimit = 1000`,
`jellycompat/content_direct.go`; native 100), so no single call dumps 100k. But the
general paged browse (`handleBrowseItems` → `BrowsePage`) is **uncached and
unthrottled** — every app-start re-runs fresh SQL, and full-page requests add a
per-page `COUNT` over the filtered set. The `Latest`/hub rails *are* cached (15-min
shared resolved-list cache + singleflight), but that's a rail, not the full list.
Nothing attributes listing load per client/device. **Detection ❌ / Enforcement ⚠️
(first page cheap & capped; full enumeration pays full price each start).**

**E22. Thundering herd on cold cache.**
For `Latest`/hub rails, `singleflight` collapses concurrent identical builds into
one DB hit. For the **general browse there is no singleflight and no cache** — N
concurrent full-library enumerations run N independent SQL workloads, bounded only
by the shared pool (`userdb.pool_max_open`, default 500), beyond which requests
queue/time out rather than shed gracefully. **Detection ❌ / Enforcement ⚠️ (only
the cached rails are protected).**

**E23. Page a 100k+ library as fast as possible.**
Each page is capped at 1000 rows, but nothing caps the number of pages, the page
rate, or aggregate cost; deep pages incur a per-page filtered `COUNT` and rising
`OFFSET` cost. No rate limit on `/Items` (compat router mounts no throttle).
**Detection ❌ / Enforcement ❌ (beyond the per-page size cap).**

**E24. Expensive sorted/filtered views over the whole library.**
Some sorts are index-optimized (`recently_added` walks a dedicated index; the
multi-library case fans into per-library index walks + in-memory merge). But
arbitrary client sorts map through `mapSortBy` — `SortBy=DateCreated` "forces a
full-library top-N heapsort" (code comment), and genre/name-prefix/person filters
add WHERE/JOIN predicates with no cost cap beyond the LIMIT, plus a full-filtered
`COUNT` per page when `include_total` is on (default). No query-cost estimator, no
cache for filtered browses, no rate limit. **Detection ❌ / Enforcement ⚠️
(query-shape-dependent).**

**E25. Badly-coded client retry storm.**
`main` *does* ship `internal/ratelimit/` (token-bucket, default-on): global 1000
rps + per-IP 120 rps, mounted inside the **authenticated native group**
(`api/router.go`, after `RequireAuth`). So a storm against an authed native
endpoint is shed per-IP. But the limiter runs *after* auth, the entire
**jellycompat surface is unthrottled**, `/auth/refresh` has no limiter, the Redis
backend **fails open** on error, and the default memory backend is per-process (so
"global 1000 rps" is really per-instance). Attribution + telemetry exist
(Prometheus by path/status, access log with `client_ip`). **Detection ✅ /
Enforcement ⚠️ (native authed only).**

**E26. Tight-loop polling `/auth/refresh`.**
Registered as a plain public route (`api/router.go`) with **no** `AuthEndpointHandler`
and **outside** the authenticated group — it hits neither the global, per-IP, nor
per-endpoint limiter. A tight refresh loop is unthrottled. **Detection ✅ (logged) /
Enforcement ❌.**

**E27. Credential stuffing via compat `/Users/AuthenticateByName`.**
Native `/api/v1/auth/login` is capped at 20/min/IP via `AuthEndpointHandler`. The
Jellyfin-compat login has **zero throttling** — the compat router mounts no limiter
— and there is no account-level lockout/backoff anywhere. **Detection ✅ /
Enforcement ❌ (compat path wide open).**

**E28. Many concurrent HTTP connections / slowloris.**
No connection cap: no `netutil.LimitListener`, no per-IP connection limit, no
handler semaphore. The `http.Server` sets only Read/Write/Idle timeouts (and the
compat + ABS servers even set `WriteTimeout: 0`). The rate limiter counts
*requests*, not *connections*. **Detection ❌ / Enforcement ❌.**

**E29. Spawn many concurrent transcodes to exhaust CPU.**
Only the per-user `max_transcodes` (default 2, `<=0` = unlimited) is checked, at
admission. There is **no per-node or server-wide ffmpeg/CPU cap** on fresh starts
(the `reconstructSem` NumCPU semaphore guards only the restart-reconstruct path).
Node scheduling has an optional `MaxJobs`/`MaxBandwidthKbps` per node
(`nodepool`), but both default to unlimited. N users at a high per-user cap — or one
user with `max_transcodes <= 0` — can saturate the box. **Detection ⚠️ (sessions
visible, no CPU/process signal) / Enforcement ❌ (no aggregate cap).**

---

## What the branch genuinely delivers vs. what it does not

**Delivers (and it's solid):**
- Authoritative, **byte-observed** stream existence that a client cannot hide by
  lying about or withholding progress (A5).
- A durable, restart-surviving, reconnect-proof **kill switch** for a *specific
  session* or a *user*, enforced on every serve surface (native, compat, edge,
  transcode node) via one shared `Refuse` + `WatchAndCut` (B9–B12, D20).
- Immediate **synchronous admission** refusal of over-cap starts, and an async
  over-cap reconciler for the multi-node picture (A1, A2, C15) — *when a per-user
  cap is set*.

**Does not cover (by scope or by gap):**
- **Under-cap re-streaming** (C14) and **token hoarding/fan-out** (C16) — no
  detection, no enforcement; the heuristic that was scoped for this is unimplemented.
- **Bulk ripping** by download (D17/D18) or sequential direct-play (D19) — the cap
  is a *concurrency* gate, not a *volume* quota; compat download is entirely
  unquota'd.
- **Library-enumeration DB load** (E21–E24) — orthogonal subsystem; per-page capped
  but per-client-uncached, unthrottled, and unattributed.
- **API floods on the compat surface, `/auth/refresh`, and connection exhaustion**
  (E25–E28) — rate limiter has real coverage gaps.
- **CPU/transcode exhaustion** (E29) — no aggregate cap.
- **The enforcer being dormant for standard users** (correction #1 / A8) — a bug
  that should be fixed before the async path is trusted.

## Recommended follow-ups (prioritized by exposure)

1. **Fix the enforcer limit source** (correction #1): resolve the group-merged
   effective cap in the `LimitFunc`, not the raw `users.max_streams` column, so the
   async reconciler actually enforces the standard cap it was built for.
2. **Quota the compat download route** (D18): bring `/Items/{id}/Download` under an
   Infuse-compatible download quota (plain GET/Range, no download rows) — today's
   only control is auth + library filter.
3. **Add a per-user *volume* budget** (D17/D19): a rolling bytes-per-period cap that
   spans downloads *and* direct-play, since concurrency caps can't bound a rip.
4. **Close VERIFY-4** (A4): treat a running transcode ffmpeg as liveness (or a
   transcode-specific grace ≥ max client buffer) so buffer-and-pause can't duck the
   cap.
5. **Extend rate limiting to the compat surface + `/auth/refresh`** and add a
   connection cap / `LimitListener` (E25–E28).
6. **Add a per-node concurrent-transcode cap** and wire `nodepool.MaxJobs` defaults
   (E29).
7. **Implement the restream heuristic** (C14/C16): the fingerprints already flow
   through `streammonitor`; a distinct-viewer-IP / distinct-title / throughput rule
   is the missing consumer. Ship disabled, tune against real traffic.
8. **Per-client listing-load accounting** (E21–E24): attribute browse cost per
   device and add caching/singleflight to the general paged browse, not just the
   Latest/hub rails.

## AI-use disclosure

This assessment was produced with AI assistance (Claude), from a read-only audit of
the `feat/sauron-async-enforcer` branch and current `main`. No code behavior was
changed. Findings cite the code as of the audit; the three corrections above are
where the branch's own plan/coverage docs overstate what the shipped code does.
