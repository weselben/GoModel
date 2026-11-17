## Rate-limit counters are per-process; no shared-state backend for multi-pod correctness

### Summary

Rate-limit counters in GoModel live **in-memory per GoModel process**, behind
`internal/ratelimit/limiter.go`'s `requests`/`tokens`/`inFlight` maps. Rule
definitions are replicated cluster-wide from the `Store` (SQL or MongoDB) at
startup and on `Service.Refresh()`, but the live counter state is not. There is
no Redis or other shared-state backend, no atomic cross-pod increment, and no
read-only probe that an edge proxy (e.g. Caddy `forward_auth`) can use to gate
on a 429.

Effect: in a multi-pod deployment, the rule `60 req/min` actually admits
`60 × N_pods` per minute per identity, because each pod grants a full quota
independently. This contradicts documented behaviour ("the rate limit applies
to the user path") and will eventually surface as a billing / abuse / SLA
bug, and as a *compliance* bug for any deployment that promised rate limits
as part of a DPA.

### Reproduction

1. Define a rule: `period: minute`, `max_requests: 60`, `scope: user_path`,
   `subject: <path>`.
2. Start two GoModel pods `A` and `B` against the same rule store
   (Postgres or Mongo).
3. Burst 60 requests through `A`, then 60 requests through `B`, with
   well-distributed `session_id` / `user_path` so neither pod's per-process
   in-flight cap trips.
4. Expected: second burst returns 429.
5. Actual: second burst returns 200 from `B`; both `A` and `B` report
   `RequestsRemaining = 0` only on their own pod.

### Why the persistence path does not save us

`Service` calls `store.SaveCounters(ctx, snapshots)` on a flush interval. That
flushes *this* pod's view of its own counters to the SQL / Mongo store. There
is no read path that lets another pod discover them at admission time.
Even if all pods flushed continuously and read on every `Acquire()`, the
race between `read → mutate → write` has no synchronisation: two pods can
both read `60`, both admit, both write `60`. The `SaveCounters` contract
("upserts the given snapshots and collects rows no writer has refreshed for
two of their own periods") is explicit that it is a single-writer design.

### What this should provide

A correct deployment needs three things; only the first is strictly required
for the issue title, but the third is what unlocks Caddy-edge gating which
seems to be the primary motivator.

1. **A shared-state counter backend.** A new `Store` implementation
   (e.g. `store_redis.go`) under the existing
   `storage.ResolveSQLBackend[Store](...)` selection, providing atomic
   `INCRBY + EXPIRE` semantics for sliding-window counters. The token /
   concurrent limiter types would map onto Lua scripts:
   - request counter: `INCR` + `EXPIRE` keyed on `(scope, subject, period, window_start)`
   - token counter: same, with `BY` amount
   - concurrent (in-flight) counter: `INCR` on admit, `DECR` on release,
     with ceiling check before `INCR`
   The `Store` interface (`Store` in `store.go`) would need
   `Admit(ctx, key, limit, period) (admitted bool, retryAfter time.Duration, err error)`
   and `Release(ctx, key)` semantics — not just snapshot persistence.
   Per-period migration is fine; per-pod caching of the last-seen value is
   acceptable as a fast path provided the source of truth is the shared
   backend.

2. **Read-only `DryAcquire` for edge/ingress gating.** A method
   `Service.Probe(subjects, now) (ProbeResult, error)` that *does not mutate*
   any counter, returning the most-constrained rule's `Limit`,
   `Remaining`, `ResetAfter` (mirroring today's `HeaderSnapshot`) plus
   `AdmitNow bool`. Caddy `forward_auth` can call this during the auth
   subrequest — alongside `/v1/auth/verify` — and reject with 429 before
   even letting the request reach GoModel. Today's `limiter.available()`
   already implements the read-only check; the gap is wrapping it in a
   stable contract and exposing it as an endpoint next to
   `/v1/auth/verify`.

3. **(Optional, but the real wedge) An endpoint for Caddy/integrations:**
   `GET /v1/ratelimit/check?subject=...&period=minute` returning
   `200` with `X-RateLimit-*` headers when there is room, `429` with
   `Retry-After` when there is not, and `401` only if the credential is
   invalid. Combine with `/v1/auth/verify` in Caddy via
   `handle_response` to flip the response code on `429` so `forward_auth`
   treats it as a rejection. Document this in `docs/guides/caddy.mdx`.

### Proposed implementation sketch (not a full PR — just to anchor review)

- `internal/ratelimit/store_redis.go`: implements `Store` for Redis with
  `Admit`/`Release` Lua scripts. `WindowSnapshot` save/load becomes best-
  effort warm-up on cold start.
- `internal/ratelimit/limiter.go`: add `Probe` that runs `available()`
  against `matchingRules(subjects)` and returns a `ProbeResult`.
- `internal/server/ratelimit_support.go` and matching test: add the
  endpoint that surfaces `Probe` as `GET /v1/ratelimit/check`.
- `config/config.example.yaml`: add `rate_limits.backend: redis` and
  `rate_limits.redis.url`.
- `docs/guides/caddy.mdx`: add the `handle_response` pattern for `429`.

### Why this matters for the project

This issue is the difference between GoModel being a development-grade
gateway and being deployable in front of real production traffic in
anything resembling a regulated environment. Until this lands, users who
front GoModel with a load balancer and expect "60 req/min per tenant" are
getting the wrong number silently. A user's Caddy `/v1/auth/verify` edge
filter also cannot reject on rate-limit pressure today — only on auth.

### Suggested labels

`area: ratelimit`, `severity: high`, `type: correctness`
