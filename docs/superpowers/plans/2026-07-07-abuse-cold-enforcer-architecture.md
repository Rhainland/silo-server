# Abuse Cold-Enforcer — Architecture Decision & Plan

> **Branch:** `abuse-cold-enforcer` (based on `origin/main`).
> **Name meaning:** "cold" = every decision stays **off the hot path**; "abuse" =
> the scope widens from concurrency-only (the `feat/sauron-async-enforcer` branch)
> to the full set of abuse classes catalogued in
> [`../../architecture/stream-abuse-matrix.md`](../../architecture/stream-abuse-matrix.md).
> Commands assume the repository root is the cwd.
>
> **Inputs learned from:** the `feat/sauron-async-enforcer` branch (kill-switch
> implementation), its plan
> [`2026-07-04-stream-monitoring-and-kill-switch.md`](2026-07-04-stream-monitoring-and-kill-switch.md),
> its coverage matrix
> [`../../architecture/playback-paths-monitoring-kill-matrix.md`](../../architecture/playback-paths-monitoring-kill-matrix.md),
> and the adversarial assessment
> [`../../architecture/stream-abuse-matrix.md`](../../architecture/stream-abuse-matrix.md).

---

> **DECISION UPDATE (2026-07-07, supersedes §2.3 and P2 below).** After adversarial
> review, the two-tier lease was **dropped**. Everything a lease provides is already
> latent in the deny-list pipeline: enforcer revocations carry a short self-healing
> TTL and are re-asserted every tick (reversibility + continuous re-evaluation), and
> the one genuine lease benefit — surviving a sustained outage — is captured by
> **reason-scoped revocation TTLs** (deterministic violations like a byte-budget
> breach write a durable revocation until the period rolls; over-cap keeps the short
> self-healing TTL; admin kills stay 24h + durable). Fail-closed enforcement was
> judged wrong for its only distinctive feeder (untuned heuristics — false positive +
> Redis blip would kill legitimate playback), and a second enforcement mechanism with
> a second failure posture was judged more fragile than the coverage it buys (one
> matrix row, B13, during a >5m Redis outage). **The architecture is the single
> pipeline: `streammonitor` → enforcer tick → `streamrevoke` → in-memory `IsRevoked`,
> with policy checks for *new* activity at synchronous admission.** Pillars 2–4 and
> phases P0/P1/P3/P4 stand unchanged; P1's enforcement action becomes a durable
> revocation instead of a Tier-1 flag.

## 1. Non-negotiables (inherited, do not regress)

1. **Off the hot path.** The per-segment / per-byte serve path may add at most one
   local in-memory lookup. No per-request DB or Redis on the serve path. (This is
   the "cold" in cold-enforcer.)
2. **No client-facing protocol change.** Clients (Jellyfin, Infuse, Silo native)
   keep echoing the same stable stream token. Any short-lived state lives
   **server-side**, keyed by the token's `sid`/`uid`. This constraint is what
   killed the naïve short-ticket idea in the prior plan; §3 shows how to keep the
   ticket *benefits* without violating it.
3. **Reliability first / predictable under failure.** A brain/Redis hiccup must not
   silently break legitimate playback. Failure posture must be a tunable knob, not
   an accident.
4. **Reuse, don't rewrite.** The sauron branch's *authoritative, byte-observed
   monitoring* (`streammonitor`, `nodesessions`) and its *durable, reconnect-proof
   revocation* (`streamrevoke`) are good primitives. Keep them; change how the
   *decision* is made and *widen what is decided*.

---

## 2. The core question: kill-list (deny) vs short-lived tickets (allow)

The prior branch chose a **deny-list**: streams run by default; the async brain
observes abuse, writes a revocation, and the edge refuses on an in-memory
set-membership check. The rejected alternative was **short-lived tickets**: the
credential itself expires quickly and must be continually re-minted, so an
unauthorized stream dies when its ticket lapses.

### 2.1 Why the branch rejected tickets — and why that reasoning is incomplete

The plan rejected tickets because Silo's `master.m3u8` is **VOD, fetched once**, so
a short ticket embedded in segment URLs would expire mid-movie and 401 the rest of
the stream; and flipping the global token TTL is a break-all-playback change.

Both objections are about putting the short TTL **in the client's credential**.
They do **not** apply if the short-lived state lives **server-side**. The client
keeps its stable 24h token; the *server* holds a short-lived **lease** keyed by that
token's `sid`. The edge hot path checks the local lease, not a client-presented
ticket. This is a "ticket" in every property that matters, minted server-to-server,
requiring zero client cooperation — which is exactly the gap the prior analysis
missed.

### 2.2 Deny-list vs lease (server-side ticket) — honest trade

| Property | Deny-list (current branch) | Lease / server-side ticket |
|---|---|---|
| Default state | **allow** (runs unless revoked) | **deny** (runs only while leased) |
| Hot-path cost | in-memory set lookup | in-memory `sid→expiry` lookup (identical class) |
| To stop a stream | write a revocation, propagate | **stop renewing** (or expire) the lease |
| Stop latency | detect → decide → propagate (~120s) | ≤ one lease TTL, **same path for every reason** |
| Maintenance load | write **only on kill** (rare, cheap) | **renew every active stream every TTL** (continuous) |
| Restart-resurrection | **must** bolt on a durable mirror (branch's GAP-4) | **free**: a reconstructed session has no lease until re-granted |
| Failure posture | **fail-open** — brain/Redis down ⇒ new kills don't land, abuse continues, but playback survives | **fail-closed** — brain/Redis down ⇒ leases lapse, abuse stops, but legit playback also stops |
| Policy checkpoint | scattered: each rule writes its own kill | **one** checkpoint: the renewal tick evaluates cap + ban + byte-budget + throughput at once |

The two are duals. Deny-list optimizes **availability** (fail-open, cheap,
already-built) and is *reactive*. Lease optimizes **abuse control** (fail-closed,
self-expiring, restart-safe, one uniform action) and is *preventive* — at the cost
of continuous renewal load and an availability blast-radius if the brain stalls.

### 2.3 Decision: a **two-tier** enforcer (deny by default, lease for suspects)

Neither pure form is right for a media server that must not break legit playback
*and* must reliably stop abuse. Use both, graduated by suspicion:

- **Tier 0 — normal streams: deny-list default (keep the branch as-is).** A stream
  runs unless explicitly revoked. Hot path = the existing `IsRevoked` set lookup.
  Fail-open. This preserves "playback never breaks because the brain hiccuped" for
  the 99% case, honoring *reliability first*.
- **Tier 1 — flagged/suspect streams: promote to lease-required (fail-closed).**
  When the cold brain flags a session — over a *soft* cap, anomalous throughput /
  distinct-viewer fan-out (re-streaming), or over a rolling byte budget — it
  **demotes** that session to lease-required: the edge now additionally requires a
  fresh central lease for that `sid`, and the brain grants it only while policy
  permits. Stop = simply stop renewing. This gives suspects the lease's good
  properties (≤TTL cut, no restart-resurrection, and — crucially — **an outage can
  no longer be used to escape an existing flag**, because a lapsed lease fails
  closed) while the expensive renewal load is paid *only for the small suspect set*,
  not every stream (honoring *performance first*).
- **Immediate admin kill stays deny-list + durable.** "Cut this now" must not wait
  ≤TTL, so admin terminate/ban writes an immediate revocation and a **durable ban
  ledger** (Postgres) so a banned *user* is never re-leased or re-admitted across a
  restart. The durable store thus shrinks from "mirror every ephemeral kill" to
  "record standing bans + quota ledgers" — a more natural durability boundary.

This is the headline architecture. Everything below is the machinery to feed it and
the adjacent pillars the matrix demands.

**What this reuses from `feat/sauron-async-enforcer`:** `streammonitor` (the
authoritative cross-node snapshot) is the input to the renewal tick unchanged;
`streamrevoke` becomes the Tier-0 + admin path unchanged; the pub/sub + Redis
`silo:sessions:*` transport is reused to carry a central-stamped `lease_until` field
(the lease is one extra field on records the brain already reads). The edge check
flips from `if revoked → refuse` to `if revoked → refuse; else if leaseRequired[sid]
&& now > leaseUntil[sid] → refuse`. Small, additive, off hot path.

---

## 3. The four capability pillars (mapped to the matrix)

The "kill-list vs ticket" decision only governs Pillar 1. The matrix demands three
more, independent of that choice.

### Pillar 1 — Concurrency & policy decision engine (A1–A8, C15)

- **Fix the dormant-enforcer bug first (matrix correction #1).** The sauron
  enforcer reads the *raw* `users.max_streams` column, which migrations set to `0`
  for standard users (real cap lives in the Default Group). `limit<=0` ⇒ skip, so
  the async brain never fires for a normal user. Resolve the **group-merged
  effective policy** (`access.EffectivePolicyForUser` → `strictestPositive`) in the
  brain's `LimitFunc`, exactly as synchronous admission already does.
- **Keep synchronous admission** (`ErrTooManyStreams`) as the immediate preventive
  gate (A1/A2). The cold brain is the cross-node backstop (A7/A8/C15) and now the
  lease authority for suspects.
- **Multi-replica (A7, VERIFY-3):** have integrated replicas publish their local
  sessions into the shared `silo:sessions:*` picture (or elect a single brain
  leader) so the cross-node count is authoritative. The lease grant is the natural
  serialization point.

### Pillar 2 — Liveness & anti-evasion (A4, A5)

- **A4 buffer-ahead-then-pause is the one open monitoring hole.** Fix it
  model-independently: treat a **running transcode ffmpeg as liveness** (or a
  transcode-specific grace ≥ the client's max buffer) so a session that pre-buffers
  and pauses fetches >45s is neither reaped nor decounted, and cannot free a slot to
  admit another. This touches the sensitive session-reaping path (cf. #279) — do it
  behind a flag with tests.
- **A5 progress-withholding is already solved** (existence is byte-observed). Keep;
  add a regression test so it stays that way.

### Pillar 3 — Volume/quota accounting (C16, D17–D19) — new

Concurrency caps count *streams*, never *bytes*, so every rip vector walks straight
through them. Add a **rolling per-user byte budget** that spans **both** downloads
and direct-play/stream egress:

- Meter served bytes per `uid` (the edge already meters egress; attribute it per
  session — the sauron branch added `BytesServed` to `SessionInfo`, reuse it) and
  per download (already tracked).
- A rolling `bytes-per-period` ledger (durable, alongside the ban ledger). When a
  user exceeds it, the brain flags them → Tier-1 lease-required → new streams/
  downloads are refused and in-flight ones cut (`WatchAndCut`) ≤TTL. This is the
  only thing that stops **D19 sequential direct-play ripping** and **C16 token
  fan-out that manifests as sustained egress**.
- **Close the compat-download hole (D18, widest gap):** route
  `/Items/{id}/Download` through the same `QuantityLimiter` + byte budget as native
  downloads (see §4 — today it has *no* quota at all). This is the single
  highest-value fix in the whole plan.

### Pillar 4 — Detection heuristics & perimeter (C14, E21–E29) — partly new, partly gap-closing

- **Re-streaming heuristic (C14/C16) — build it (it does not exist; matrix
  correction #2).** The fingerprints already flow through `streammonitor`
  (`ClientIP`, `ClientName`, `MediaFileID`, `BytesServed`). Add a consumer rule:
  distinct client-IPs per `sid`, sustained throughput vs. media bitrate, distinct
  concurrent titles per `uid`. Output = a flag → Tier-1 demotion. Ship **disabled**;
  tune against real traffic before enabling (false-kill risk).
- **CPU/transcode exhaustion (E29):** add a **per-node concurrent-transcode cap**
  at the node's `handleStart` admission (today `reconstructSem` guards only the
  restart path; fresh starts are unbounded). Wire `nodepool.MaxJobs` sensible
  defaults.
- **API-flood perimeter (E25–E28):** extend the existing `internal/ratelimit` to
  the **jellycompat surface** and `/auth/refresh` (both currently unthrottled), and
  add a connection cap (`netutil.LimitListener` / per-IP). These live in existing
  subsystems — gap-closing, not new architecture.
- **Library-list load (E21–E24):** out of scope for the stream enforcer, but the
  cold philosophy applies — add per-client browse-cost accounting + a cache/
  singleflight on the *general* paged browse (today only the Latest/hub rails are
  cached). Track separately; note here so it is not forgotten.

---

## 4. Download-quota verification (answering the explicit question)

**Claim under test:** the admin "max concurrent downloads per user" / "max downloads
per period" / "period duration" settings let an operator cap e.g. **20 downloads per
30 days**, and that cap actually holds.

**Result: it HOLDS for the native download routes, and is BYPASSED on the compat
route.**

- Config keys exist and are operator-settable: `download.max_concurrent_per_user`
  (default 3), `download.max_per_period` (default **0 = unlimited**),
  `download.period_duration` (default 24h) — `internal/config/db_loader.go:545–553`.
  Setting `max_per_period=20`, `period_duration=720h` gives "20 per 30 days".
- The period check is enforced **at download-record creation** at all four creation
  sites (single, direct, series batch, season batch) —
  `internal/downloads/service.go:356,427,597,689` call `limiter.Check(...)` *before*
  inserting rows.
- **Crucially, the period count is not defeatable by completing downloads.**
  `CountByUserSince` counts every row with `created_at >= now-period AND status NOT
  IN ('cancelled','failed')` (`internal/downloads/repo.go:190–200`) — i.e.
  **completed downloads still consume the period quota**. Only the *concurrent*
  gate (`CountActiveByUser`, `status IN ('queued','downloading','preparing')`,
  `repo.go:176–186`) frees on completion. So a `create→complete→create` loop can
  keep 3-in-flight forever but **cannot** exceed 20-in-30-days. Cancelled/failed are
  excluded so transient failures don't burn quota — correct.

**Caveats the operator must know:**

1. **Compat `/Items/{id}/Download` bypasses all of it.** `HandleDownload`
   (`internal/jellycompat/streams.go`) creates no download row and never calls
   `QuantityLimiter` (grep confirms zero references). An Infuse-style client
   downloads unlimited full files regardless of the 20/30d setting. → Pillar 2/§3
   fix: route it through the same limiter + byte budget.
2. **The period quota counts downloads, not bytes.** "20 downloads" of 20 different
   4K remuxes is ~1 TB; the quota bounds *count*, not *volume*. The Pillar-3 byte
   budget is what bounds volume.
3. **Streaming-based ripping is unaffected.** Sequential direct-play (D19) creates
   stream sessions, not download rows, so the download quota never sees it.
4. **Minor TOCTOU:** two concurrent `Check` calls can both pass before either
   inserts; the batch path mitigates via `batchSize`, but a burst of single
   creates could momentarily overshoot by the concurrency of the burst. Low
   severity; note for a follow-up (advisory lock or insert-then-verify).

**Bottom line for the operator:** yes, 20/30d works today for the native app's
downloads and can't be gamed by finishing downloads — but it does **not** cover
Infuse/compat downloads or streaming rips, and it counts files not bytes. The plan
closes those three gaps in Pillars 2–3.

---

## 5. Matrix coverage after this plan

| Matrix rows | Today (sauron branch) | After this plan |
|---|---|---|
| A1–A3 over-cap | ✅ admission (once bug #1 fixed) | ✅ + reliable cross-node backstop |
| A4 buffer-ahead | ❌ invisible window | ✅ ffmpeg-liveness (Pillar 2) |
| A5 progress-withhold | ✅ | ✅ (+ regression test) |
| A6–A8 churn / multi-replica / dormant enforcer | ⚠️ | ✅ bug-fix + replica publish + lease-for-suspects |
| B9–B12 admin kill/ban/restart | ✅ | ✅ (durable ban ledger, unchanged) |
| B13 Redis outage | ⚠️ fail-open (abuser escapes) | ✅ flagged suspects fail-closed; legit fail-open |
| C14 under-cap re-stream | ❌ | ⚠️→✅ heuristic (Pillar 4, ship disabled) |
| C15 over-cap re-stream | ✅ | ✅ + labeled |
| C16 token hoard/fan-out | ❌ | ⚠️ caught via byte budget when it manifests as egress |
| D17 native rip | ⚠️ count-only | ✅ byte budget (Pillar 3) |
| D18 compat rip | ❌ no quota | ✅ route through quota + budget |
| D19 sequential direct-play rip | ❌ | ✅ byte budget |
| D20 admin stop rip | ⚠️ WatchAndCut | ✅ (unchanged) |
| E21–E24 library load | ❌ | ⚠️ browse-cost accounting + cache (tracked separately) |
| E25–E28 API floods | ⚠️ gaps | ✅ extend ratelimit to compat/`/auth/refresh` + conn cap |
| E29 CPU exhaustion | ❌ | ✅ per-node transcode cap |

---

## 6. Phased plan

1. **P0 — correctness first (small, high value):** fix the dormant-enforcer limit
   source (matrix #1); close the compat-download quota hole (D18); add the per-node
   transcode cap (E29). No new architecture — all three are bounded fixes with
   outsized payoff.
2. **P1 — byte budget (Pillar 3):** rolling per-user bytes-per-period ledger across
   downloads + stream egress; wire into a flag. Bounds all rip vectors.
3. **P2 — two-tier lease (Pillar 1):** add the `lease_until` field + Tier-1
   demotion for flagged sessions; fail-closed for suspects, fail-open for normal.
4. **P3 — liveness (Pillar 2):** ffmpeg-as-liveness for A4, behind a flag + tests.
5. **P4 — detection (Pillar 4):** re-streaming heuristic (ship disabled) + perimeter
   ratelimit gap-closing + library-browse cost accounting.

Each phase is independently shippable and independently valuable; P0 alone closes
the three worst holes.

## AI-use disclosure

Drafted with AI assistance (Claude) from a read-only audit of both branches and the
four referenced docs, plus targeted verification of the download-quota enforcement
path. No code behavior changed by writing this plan.
