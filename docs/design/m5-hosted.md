# M5 design: hosted, multi-tenant and multi-node foundations

Status: agreed design (CMTG-32); nothing here is implemented. This page sets out what the hosted product is, which engine changes it needs, and the order to build them in. M4's exit criterion is not fully met yet ([M4](m4-logical-feed.md#exit-criterion-check-cmtg-92-2026-10-18)); nothing in M5a depends on closing it, but the hub (M5b) builds on the logical feed and waits for it.

## 1. The hosted product

CometGo is a library linked into the customer's Go binary. Their methods and publication `fn`s are their code, run in their process, against their schema. That settles most of the product's shape.

**What the customer runs.** Their app, with CometGo in it, and their own Postgres (any provider from [M4 §9](m4-logical-feed.md#9-managed-postgres)). Observer re-runs land on their database and use its CPU, exactly as when they self-host. If M5a's measurement shows it is needed (§3.1), they also run the hub, a single open-source binary, inside their own network.

**What the hosted service runs: operations and observability, not the data path.**

1. **Database and feed checks.** `wal_level`, slot and sender headroom, `max_slot_wal_keep_size`, replica identity, slot lag and retained WAL, hub and node health, with alerts.
2. **Dashboards.** Per-publication and per-tenant cost and freshness (§5), from the nodes' metrics.
3. **Support**, and later a vendor-run hub for customers who accept what it implies (§3.6).

**Why not the data path.** The logical feed decodes whole rows by default ([M4 §2](m4-logical-feed.md#2-what-is-decoded)), so a hub run by the vendor receives every published row of the customer's data and holds a replication-role credential. That is a data-processing agreement, a compliance review (SOC 2, residency) and an inbound firewall rule before a customer can try it, and it puts a network hop to the vendor inside every fence. A control plane that receives only metrics and health avoids all of that. The honest consequence: what is paid is alerts, dashboards, checks and support. Under the proposal in §7 (pending the owner's decision), everything that makes CometGo correct and scalable stays open source.

**What it does not run.** Not the customer's Go code (methods, publication queries, WebSocket termination), and not their Postgres. Hosting the app makes CometGo a general Go PaaS, a different business; hosting the WebSockets alone needs the engine split across a network boundary every tick crosses. Out of scope for M5.

**Tenancy, two meanings.**

- *Customer tenancy:* the control plane serves many customer apps. It is shared; each app's data path is its own.
- *In-app tenancy:* a customer's app serves its own tenants (a SaaS with organisations). CometGo has the pieces (`ScopeKey`, `ScopeShared` with the tenant in the args); M5 adds tenant routing (§4) and tenant metrics (§5).

**What the control plane receives.** Metric values with publication, method and tenant labels. Tenant ids are hashed by the agent by default (an option sends them in clear for customers who want readable dashboards). Nodes authenticate with an app key that can be rotated.

**SLOs of the hosted service** (the customer's app keeps its own; the control plane being down never affects it):

| What | Target |
|---|---|
| Ingest: a metric visible on a dashboard | within 60 s, 99% of the time |
| Alert: a slot over its WAL threshold, a hub or feed down | raised within 2 min |
| Control plane availability | 99.9% per month |

Still open for the M5c design: the billing unit (per app, per node or per connection), and multi-region ingest.

## 2. Per-tenant engines: no

The concern: one engine goroutine per `App`, so one tenant's slow query could stall the others. M3 answered most of it inside one engine: slow queries detach, slow `fn`s are carried, fences are per connection. What one engine still shares across tenants is real:

- `MaxDetached` defaults to `min(MaxConns − 4, 2)` (app.go), so two slow observers of one tenant take every detach slot, and known-slow observers of other tenants are deferred until one frees.
- `FnWorkers` (8) is shared: one tenant's burst of slow `fn`s queues every other tenant's subscribes behind it.
- The pool is shared.

Engines per tenant would not fix this cheaply: each needs a pool share, a tick loop and a feed (a slot each on the logical feed, or a hub connection each), and hundreds of them do not fit. So:

- **No per-tenant engines.**
- **Per-tenant caps instead**, on detached queries and on `fn` workers held by one tenant. They are foreseeable from the above, not speculative, but they need tenant attribution (§5) first. Planned for after M5a, once the metrics show the shape.
- Several Apps in one process stay possible (nothing is global); it is not the tenancy model.

## 3. Multi-node: per-node slots, then a hub

### 3.1 Options

| | Per-node slots (today) | Hub fan-out | Observer ownership |
|---|---|---|---|
| WAL decoded | once per node | once | once |
| Slots, senders | one per node | one per app | one per app |
| Gate snapshots | ~1,000–1,500/s per node (CMTG-92) | once | once |
| Observer re-runs | every node holding it | every node holding it | once |
| New machinery | none | hub, hub↔node stream | membership, rebalancing, forwarding results and fences, failover |

Ownership is the only option that cuts re-run load per node, and it is the expensive one. Sticky routing by tenant (§4) gets most of that saving cheaply: if a tenant's users land on one node, that tenant's observers exist on one node. **Ownership is not in M5.**

**The hub is not justified yet.** Nothing so far shows per-node decoding hurts below the default ten slots. M5a measures it (§8): decode CPU, gate snapshots per second and retained WAL for N per-node slots, at N = 2, 5 and 10. The hub is built (M5b) when that shows a cost worth removing, or for apps that need more nodes than `max_replication_slots` allows. The rest of this section is its design, reviewed so that the decision is only "when".

### 3.2 The hub

A process (`cmd/comethub`) that runs the M4 decoder and gate unchanged (slot, resume, zombie handling, visibility gate, streaming of large transactions), and instead of handing visible transactions to an engine, appends them to a **log** and streams the log to every connected node.

- **Log entries** are the gate's output: one per visible transaction, its notifications in order, each notification with its epoch-extended 64-bit xid (never the 32-bit one), stamped with the transaction's commit end LSN. Restart entries carry the whole snapshot S (xmin, xmax, xip). The hub's second pass (M4 §6, `secondPass`) is a log entry too, not a direct call.
- **Cursor.** A node's position is `(hubEpoch, seq)`, where `hubEpoch` is random per hub process and `seq` numbers entries; restart entries have no LSN of their own. Each entry also carries its commit LSN, for resume across hubs (§3.4).
- **Retention.** The last `HubRetain` (60 s, bounded by bytes) of entries, in memory.
- **Confirming** as the M4 feed: what was received in full, not what nodes acknowledged. A node behind retention takes the restart path, which is always correct, and the slot never holds WAL for a slow node.
- **A slow node never blocks the hub.** A node whose send buffer passes the retention bound is disconnected and takes the restart path.
- **Everything goes to every node.** Fences carry their node in the token (`f|<node>:<n>`); routing them is a later optimisation.
- **Protocol version** in the handshake from day one: nodes and hubs of different releases meet during every rolling deploy.

### 3.3 The node side: `FeedHub`

A third `Config.Feed`, with `Config.HubAddr`.

- **Fences, events, re-checks** are the logical feed's statements, transactional `pg_logical_emit_message` in the method's transaction. The node needs no replication connection, and `Config.DB` may go through a transaction-mode pooler.
- **Connect.** The node sends its cursor and the commit LSN of the last entry it applied. The hub resumes it when it holds every entry after that point: the same `hubEpoch` and `seq` still retained, or (after a failover, §3.4) a commit LSN at or above the point the hub's log covers from.
- **Otherwise, a restart.** The hub picks the "from now" position **under the lock that serialises its log appends**, and acknowledges with it. The node takes snapshot S only after it receives that acknowledgement, and buffers entries that arrive between the acknowledgement and S. It then follows M4 §6: everything dirty, every sub re-checked, fences visible in S released, and buffered or streamed transactions visible in S dropped (otherwise an event could follow its `res`). A transaction not visible in S became visible after S; the hub sends a transaction only after a snapshot shows it, and visibility is monotonic, so it was appended after the acknowledgement and arrives. M4's second pass is not needed on a node reconnect: that gap belongs to a slot's consistent point, and the node's stream has none.
- **Visibility.** Any snapshot the node takes after receiving a transaction is later than the hub's snapshot that released it, so it sees the transaction. This holds only while the node's `Config.DB` and the hub reach the same primary; during a database failover both reconnect, and the hub's new slot sends a restart entry.
- **The hub's feed state is the node's.** A hub that is not streaming (no slot, slot in use by another hub, replication connection down) tells its nodes, and they report the feed down exactly as when their own slot is down.

**Readiness.** Today `Ready()` fails 10 s after the node's feed goes down, so a Postgres restart does not take every node out at once. With a hub, every node's feed is the same one: a hub outage over 10 s would take every node out of the load balancer together, and the customer's whole app with it. So under `FeedHub`, `Ready()` drops only the 10 s down-grace clause: a node whose feed went down after being up stays ready. It keeps the startup clause (not ready until the feed has been up once), so a rolling deploy during a hub outage stalls and the old pods keep serving, instead of replacing them with nodes that serve a first snapshot and never update; the cost is that a scale-up during a hub outage adds no capacity. `Ready()` gains no database check: one would fail on every node at once during a Postgres restart, the case the grace exists for. While the feed is down, subscriptions are stale and fenced calls answer with `lag:true` after `FenceTimeout`; `FeedErr()` reports the feed's state for monitoring and alerts. A stale but serving app is better than no app.

### 3.4 Hub availability

- **One active hub per app;** a standby waits for the slot. A hub **accepts and keeps nodes only while it streams**: a standby in "slot in use" backoff (logicalfeed.go) refuses nodes, so nodes behind a `HubAddr` list find the active one.
- **The hub never drops its slot on shutdown.** The logical feed drops its slot on a clean `Shutdown` (logicalfeed.go); a hub reusing that would drop it on every deploy of the hub, and the standby would start from a new slot, the restart path for every node. Removing a hub's slot is an explicit admin command.
- **Hub slots are named `cometgo_hub_<name>`**, and the nodes' orphan sweeper skips them: a node sweeps every inactive `cometgo_*` slot of its database but its own (sweeper.go), which would include a hub slot left inactive across a deploy.
- **Leases.** A hub that had nodes and then lost all of them for `HubOrphanAfter` (30 s), while still holding the slot, ends its replication connection, so a standby the nodes can reach takes over. Without this, a hub cut off from its nodes but not from the database keeps the slot forever. The lease does not fire for a hub that never had nodes (first start, scale-to-zero), and only when a standby is configured: a lone hub keeps streaming and confirming, so the slot retains no WAL meanwhile. After releasing, a hub waits at least `HubOrphanAfter` before trying for the slot again, so two hubs do not trade it back and forth. On Neon, the hub's connection keeps the compute awake (M4 §9).
- **Failover without a storm.** The new hub resumes the slot from `confirmed_flush` C, and Postgres sends every commit ending above C, so its log is complete from C. It reads C as `confirmed_flush_lsn` after acquiring the slot and advertises "covers from C". A node whose last applied commit LSN L is at or above C resumes without loss; only a node below C restarts. This is sound although the old hub confirmed what it *received* (`confirmable`, including keepalive positions), which can pass commits still held in its gate: delivery is in commit order, so a node that applied L has every commit ending at or below L (or covered by its own restart snapshot), and a commit held in the old gate ends above L, so C is above L and that node restarts. A new slot's consistent point is above every old L, so then every node restarts. In steady state the slot is confirmed about every second, so after a failover nearly every node resumes instead of re-running every observer at once; after a stall (a synchronous standby wait holding the gate) many nodes will be below C. (To verify in the hub task: that a resume from C sends every commit ending above C, which M4 §5 relies on as well.)

### 3.5 What it costs the customer

One more process (two with a standby) in their network, one replication connection from it, and nodes connecting to it. Fewer slots and senders than one per node.

### 3.6 A vendor-run hub, later

The same binary run by the vendor, for customers who accept sending row data out (§1). It needs a data-processing agreement, same-region placement (the hop is inside every fence), private networking options, and per-customer hub processes so that one customer's large transaction never delays another's. Not before M5c.

## 4. Sticky routing

Session resume needs reconnects to reach the node that holds the session ([server.md](../server.md#session-resume)), and tenant routing is what makes §3.1's re-run saving work. A load balancer routes on the HTTP request, so the key must be on the WebSocket URL.

- **Client.** A `routeKey` option on `connect`, appended as the `route` query parameter with the `URL` API (a user-supplied `url` may already have a query). The app sets it to the tenant or user id. When unset, the client generates a random id once per client instance and keeps it across reconnects, so connections without a key still spread out, and resume still reaches the same node.
- **Server.** Ignores it. A key that routes badly costs only observer sharing; a reconnect that lands on another node resyncs, as today.
- **Docs.** Load balancer recipes for hashing on it (nginx `hash $arg_route consistent`, Envoy ring hash, HAProxy `hash-type consistent` on the parameter), what happens when a node leaves (its sessions resync elsewhere), and two warnings: the key is chosen by the client, so a client can pile connections onto one node (`MaxConns` and `MaxConnsPerIP` bound the damage per node), and it lands in access logs, so use an opaque id rather than an email address.

## 5. Per-tenant and per-publication metrics

Today `Metric` has `Pub`. Tenant attribution:

- **Which tenant.** `Config.Tenant func(*Identity) string`, optional; it defaults to `Identity.ScopeKey`, which is already documented as the tenant partition. It is evaluated at bind and again on every identity change (login, logout, refresh to another scope), and kept on the connection.
- **`Metric.Tenant`** is set on metrics of one connection or call: fence wait and lag, slow closes, and new metrics for method calls (`call_seconds`, with `Method`) and for publication `fn` time (`fn_seconds`, with `Pub`), which is the cost most directly attributable to a tenant.
- **Per-tenant gauges** (subscriptions, queued bytes) are reported once a second, not per tick: per tick they would cost one hook call per tenant on the engine goroutine.
- **Observer runs** (`pub_run_seconds`, `stale_seconds`) get a tenant when every sub of the observer has the same one, else `TenantMixed` (`"*"`). The observer keeps a tenant and a sub count per tenant, updated as subs attach, detach and rebind, so a run reads one field instead of scanning its subs. `ScopeUser` observers with a `ScopeKey` and `ScopeShared` ones keyed by a tenant argument are single-tenant, the common case.
- **Cardinality** is the hook's problem: the engine passes values, an exporter decides what to label.

The hosted agent is a `Metrics` hook in the customer's app that aggregates per interval and sends to the control plane.

## 6. Read replicas

Not in M5. Noted because it decides who pays for re-runs. A standby makes a transaction visible when it replays its commit record, in WAL order, and the feed delivers in commit order too. So a node could tick on a standby once the standby's replay LSN has passed the last entry drained, keeping the snapshot and fence rules. To be verified (standby snapshots, `hot_standby_feedback`, the lag it adds to every fence) before it is designed.

## 7. Open source and paid

M4 decided both feeds are open source (M4 §11). For M5, the proposal:

- **Open source:** `FeedHub`, the hub↔node protocol and `cmd/comethub`, routing keys, tenant metrics, and the metrics hook the agent uses. A customer can run all of it without the vendor.
- **Paid:** the control plane (checks, alerts, dashboards), support, and later the vendor-run hub.

A closed hub would keep multi-node scaling past ten nodes out of open source; open-core on a correctness-critical component invites a fork and makes the open version look capped. **Decision needed from the owner.** M5a does not depend on it, and the hub is built in this repository either way.

## 8. Milestones

**M5a: tenant routing, tenant metrics, and the multi-node measurement.** Small, independent of the hub, and useful at once.

Exit criterion:

- the client's `routeKey` and the documented recipes route a reconnect to the same node behind nginx with consistent hashing (an integration test with two nodes),
- `Metric.Tenant` is set on the metrics listed in §5, with tests for the single-tenant, mixed and rebind cases,
- feedbench runs N per-node `FeedLogical` apps (`-nodes N`, each with its own `SlotName`) and reports, per run: write throughput; walsender CPU from `/proc/<pid>/stat` (utime + stime) for the slots' `pg_replication_slots.active_pid`, sampled at start and end (possible against the compose Postgres, not managed providers); decoded and spilled bytes from `pg_stat_replication_slots`; retained WAL as `pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)`; and gate snapshots per second, from a new counter on the feed. Recorded in `docs/bench/` for N = 1, 2, 5, 10 on the idle baseline machine, with `max_replication_slots` and `max_wal_senders` raised in the compose config and stated in the report.
- **Decision rule, set now:** the hub (M5b) goes ahead if write throughput at N = 10 is more than 10% below N = 1, or walsender CPU at N = 10 exceeds one core; otherwise it waits for an app that needs more nodes than the slot limit.
- The routing test needs an nginx service in compose and CI, as PgBouncer got one (`make test-pgbouncer`).

**M5b: the hub**, if M5a's measurement justifies it (§3.2–§3.4). Exit criterion: 20 nodes on one database through one hub with the logical feed's fence, visibility and restart tests passing under `FeedHub`; fence wait p99 through the hub measured with cometload; a hub kill and failover recovers every node within 30 s, with no lost re-check (a chaos test) and only nodes behind C restarting.

**M5c: the hosted control plane** (a separate, private repository): metrics ingest, checks and alerts, dashboards, billing. Its own design.

**Later:** read replicas (§6), per-tenant caps (§2), fence routing at the hub, the vendor-run hub (§3.6), observer ownership (§3.1).

## Review

A sparring agent reviewed the design from the code over two rounds and ended at AGREE WITH CHANGES; the changes are applied above. What it forced:

- the hosted service out of the data path (§1): a vendor-run hub would receive every published row and hold a replication credential, a bigger adoption hurdle than network access,
- the hub made conditional on a measurement (§3.1, §8), since nothing showed per-node decoding hurts below ten slots,
- readiness under a shared hub (§3.3): with the old rule one hub outage would take every node out of the load balancer at once; without the startup clause a deploy during an outage would serve frozen data,
- a hub that is not streaming refuses nodes and passes its state on, leases with their edge cases, and the slot kept across a hub's own shutdown (§3.4),
- resume across a failover from `confirmed_flush` instead of a restart storm, with the argument for why it is sound (§3.4),
- the subscribe acknowledgement taken under the log's append lock, and entries before S buffered and filtered (§3.3),
- a random default routing key (§4), since hashing an empty key sends every keyless client to one node,
- per-tenant engines rejected for the right reason, the shared detach slots and `fn` workers (§2),
- a measurable M5a exit criterion with a decision rule (§8).

## Tasks (M5a)

Filed in Backlog with these dependencies:

| Task | What | Depends on |
|---|---|---|
| CMTG-101 | Client `routeKey` (random default), load balancer recipes and warnings (§4) | |
| CMTG-102 | nginx consistent-hash routing integration test in compose and CI | CMTG-101 |
| CMTG-103 | Per-tenant metrics: `Config.Tenant`, `Metric.Tenant`, `call_seconds`, `fn_seconds`, per-tenant gauges, observer attribution (§5) | |
| CMTG-104 | Counter for the logical feed gate's snapshot queries | |
| CMTG-105 | feedbench `-nodes N` with walsender CPU, slot stats and retained WAL | CMTG-104 |
| CMTG-106 | Run the measurement at N = 1, 2, 5, 10 and apply the decision rule; file M5b if the hub goes ahead | CMTG-105 |
