# M4 design: the logical-replication change feed

Status: draft, under review (CMTG-31). The wire-visible parts will go in the [protocol](../protocol.md) §11, marked **[later]**. This page holds the engine design and the rules each implementation task must keep.

**Goal.** Remove the NOTIFY ceiling on writes, and work when the app's pool goes through a transaction-mode pooler (PgBouncer, Supavisor, Neon's pooler).

**Exit criterion.** In [feedbench](../bench/README.md), on the machine that records the baselines:

- write throughput with the feed on is at least 90% of the no-feed baseline (`plain`), with and without a fence per transaction,
- the full test suite passes with `Config.DB` pointing at PgBouncer in transaction mode, and the feed on a direct connection.

## Why NOTIFY is the ceiling

Every statement on a tracked table runs a statement trigger that counts the transition table, aggregates the changed keys into JSON and calls `pg_notify` (triggers.go). Every fenced call adds a `pg_notify` (conn.go). At commit, a transaction that queued a notification takes a database-wide lock (`NotifyQueueLock`) to append to the queue, so such commits serialize. And `LISTEN` needs a session that stays on one backend, which a transaction-mode pooler does not give, so today the listener silently uses the pool's DSN.

Numbers: see [feedbench.md](../bench/feedbench.md). (A first 4-writer run on a busy machine: triggers 46% of plain, triggers plus fence 47%, logical feed 95%, logical plus message fence 97%.)

## What the spike showed

`spike/cmtg-31-pgoutput` (`cmd/feedspike`) reads a temporary pgoutput slot and produces the existing §11 payloads:

| Scenario | Payloads |
|---|---|
| insert 3 rows | `1\|r\|I\|spike.items\|[["1"],["2"],["3"]]` |
| update, key unchanged | `1\|r\|U\|spike.items\|[["1"]]` |
| update, key changed | `1\|r\|U\|spike.items\|[["2"],["100"]]` (old and new key) |
| delete | `1\|r\|D\|spike.items\|[["3"]]` |
| write, then a transactional message | the row payload, then `f\|node1:7`, in one transaction |
| non-transactional message, transaction rolled back | the message alone, delivered at once |
| 2,000 rows in one transaction | `1\|t\|I\|spike.items` |
| composite key | `1\|r\|I\|spike.comp\|[["acme",1],["globex",1]]` |
| truncate | `1\|t\|T\|spike.comp` |
| create a slot while a transaction holding an xid is open for 2 s | creation took 2.14 s |

`pg_logical_emit_message(true, …)` assigns a transaction ID, so the fence keeps its "no ID, no fence" check before it.

## Design

### 1. Two feeds, one notification stream

`Config.Feed` selects `FeedNotify` (today's, the default in M4) or `FeedLogical`. Both deliver the same `notification` values to `engine.notify`, so observers, Match filtering and fences keep their logic. In logical mode each notification also carries the **xid** of the transaction it came from (pgoutput's BEGIN), which §5 and §6 need. Everything below is the logical feed.

### 2. What is decoded

- **Publication.** `InstallPublication(ctx, db, tables...)`, the logical counterpart of `InstallTriggers`, adds each table to publication `cometgo`, publishing insert, update, delete and truncate, with `publish_via_partition_root = true` so a partitioned table reports changes under its own name, as observers list it.
- **Whole rows by default.** A key-only column list would keep the stream small, but from PG 15 an UPDATE or DELETE fails on a published table whose column list does not cover its replica identity, so a later migration that changes the key would break the app's writes. Key-only lists are an opt-in (`PublicationKeysOnly`), and `checkTables` then checks at start that each still covers the replica identity. Whole rows cost more in wide or TOASTed tables; the exit criterion is measured on such a table too.
- **Replica identity** must be `DEFAULT` (the primary key) or `FULL`, checked at start. An UPDATE that changes the key carries the old key (or the whole old row under `FULL`), so the old and new keys are reported, as today.
- **A known hazard of logical mode, with or without column lists:** Postgres refuses UPDATE and DELETE on a table that publishes updates and has no usable replica identity. A migration that drops the primary key, swaps it through a tool that does so, or sets `REPLICA IDENTITY NOTHING`, makes the app's own writes fail until the table has one again or leaves the publication. A start-time check cannot catch a migration that runs later. Triggers never had this failure mode. The docs say so where logical mode is introduced.
- **Keys** are encoded exactly as the triggers' `jsonb_build_array` does (int8 as a string, int2/int4 as numbers, text and uuid as strings; §10), with a parity test per key type against the trigger feed.
- **Per transaction**, the decoder groups keys per (op, table), and flushes them before each transactional message and at commit. More than 1,000 keys for one (op, table) becomes a table-dirty notification, as with the triggers. There is no 8,000-byte payload limit.
- **Order.** pgoutput sends each transaction at its commit, in commit order. A transaction that rolls back sends nothing.

### 3. Fences, events and re-checks as logical messages

All are **transactional** messages with prefix `comet`:

- **Fence:** `SELECT pg_logical_emit_message(true, 'comet', 'f|<token>'), txid_current() WHERE txid_current_if_assigned() IS NOT NULL`, the last statement of the method's transaction, as the `pg_notify` today. The fence keeps the xid (§6). `pg_logical_emit_message` assigns an xid, so the "no ID, no fence" check stays in front of it.
- **EmitAfterCommit / RevalidateAfterCommit:** `e|<payload>` and `v|<userID>` in the method's transaction.
- **Emit / Revalidate:** the same messages, each in its own autocommit statement. A non-transactional message waits for the WAL writer (up to `wal_writer_delay`, 200 ms by default) before it can be decoded; a commit flushes at once.

No NOTIFY is sent: commits do not serialize on the notify queue lock, and every statement is plain SQL, which works through a transaction-mode pooler.

### 4. Decoded is not yet visible

The walsender decodes a transaction once its commit record is flushed, but other sessions see it only after `ProcArrayEndTransaction`, which runs after any synchronous-replication wait. With synchronous standbys the gap is a network round trip, and unbounded while a standby is down. NOTIFY never had this gap: listeners skip entries from transactions still in progress.

So **the decoder holds each committed transaction until it is visible**, before handing its notifications to `engine.notify`, and every later transaction behind it, in commit order. It polls `pg_current_snapshot()` every few milliseconds on a **dedicated connection** (a pool saturated by method calls must not stall the feed), one query per batch, with no backoff. While it holds, the replication connection keeps being read, buffered and answered (status updates and probes included): with the short `wal_sender_timeout` of §5, a stalled reader would have its walsender ended and resume into the same held transaction, over and over. Visibility is monotonic, so every tick snapshot taken after the hand-off sees the transaction: the tick's own, its detached lanes and the Match candidate filter. `tick()`, fences, held events and carried requests need no change. A transaction stuck in a synchronous-replication wait holds this node's feed behind it, as it holds every reader that waits for it; it does not stall the engine, whose ticks, calls without writes and other work go on.

**Xids.** pgoutput's BEGIN carries a 32-bit xid; it is extended with the epoch of the snapshot's `xmax` before comparing with the 64-bit `xid8` values of `pg_current_snapshot()`, never compared raw. Subtransactions need nothing: BEGIN carries the top-level xid, `txid_current()` returns it, and a snapshot's `xip` holds top-level xids only.

### 5. The replication connection and the slot

- `Config.ReplicationDSN`: a direct connection (`replication=database`); poolers do not carry the replication protocol. Required in logical mode. `Config.DB` may then go through a pooler. The feed sets a short `wal_sender_timeout` on it (a user-settable option), so a dead connection's walsender ends quickly.
- **A persistent slot per process**, named `cometgo_<SlotName>` where `Config.SlotName` defaults to the random node ID and can be set to a stable name (a pod or host name), so a process restarted under the same name resumes its slot instead of leaving one behind. Creating a slot waits for every transaction that holds an xid (2.14 s with one 2 s transaction in the spike, unbounded in general), so it happens at start, with a timeout and backoff. Until it succeeds the node's feed is down: it logs that, and its health check reports not ready rather than serving stale subscriptions.
- **Resume without duplicates.** After a dropped connection, the node restarts replication from the **end LSN of the last commit it received in full**, not from the confirmed LSN, which trails what the engine has drained. Postgres skips commits below a requested start that is above `confirmed_flush`, so nothing is delivered twice and nothing is lost. A transaction whose BEGIN arrived without its COMMIT counts as not received. (To be verified in the slot task: the skip comes from reading `CreateDecodingContext`.)
- **A zombie walsender** from the dead connection keeps the slot active until it notices. The node records its walsender's `pg_backend_pid()` when it connects; if the slot is then active for **that** PID, it terminates it (`pg_terminate_backend`, allowed for the same role) and retries, instead of waiting out `wal_sender_timeout` while every fence passes `FenceTimeout`. A slot active for any other PID is a conflict, not a zombie: another live process uses the same `SlotName` (a rolling deploy that starts the new pod first, or a misconfiguration). The node reports "slot in use" and retries with backoff; it never terminates another process's walsender, which would let two nodes kill each other's feed in turn.
- **A new process on a stable name** resumes the slot from its confirmed LSN. Its engine starts empty, so its first tick is the usual full all-dirty pass.
- **A missing slot** (dropped by a sweep while partitioned, or its WAL removed by `max_slot_wal_keep_size`) takes the start path: a new slot, then the all-dirty pass of §6.
- **Orphans.** The slot is dropped on clean shutdown. A crashed process leaves it behind, so a periodic sweeper on every node drops `cometgo_*` slots **of its own database** (slots are cluster-wide, and another app or environment may share the cluster) that are inactive, and have been for `OrphanSlotAfter` (10 min): `inactive_since` on PG 17; before that, seen inactive with an unchanged `confirmed_flush_lsn` across sweeps that far apart. At start, a node also checks there is room for its slot in `max_replication_slots` and says which `cometgo_*` slots hold it. The docs require `max_slot_wal_keep_size` to bound what an orphan holds meanwhile.
- **Confirming.** The node confirms the commit LSN of each transaction once the engine has drained it, and, when idle and outside a transaction, the WAL end from the server's keepalive, so writes to untracked tables do not pile up behind the slot.
- **Liveness.** The node sends a status update with `replyRequested` when it has heard nothing for `listenIdle`, and reconnects if no reply comes within `listenProbe`. Server keepalives alone come every `wal_sender_timeout / 2`, which may match `listenIdle` and cause spurious reconnects.

### 6. Starting the feed (the all-dirty tick)

After a slot is recreated, changes committed before it is consistent are not decoded. (At first start no fence is pending, so this matters for a recreated slot; the rule is the same.) Today `listenMu` makes fenced commits wait while `LISTEN` runs. Holding them while a slot is created is not acceptable, and an LSN rule does not work: a transaction that wrote before the consistent point but commits after it is decoded (verified: a message at 0/FB356DE8, consistent point 0/FB356E50, committed and decoded two seconds later).

The rule is by **xid visibility in the all-dirty tick's snapshot S**, taken after the slot is created:

- A fence whose xid is visible in S is released by that tick, and any decoded message from a transaction visible in S is dropped: S already covers its writes, its event may otherwise follow its `res`, and the all-dirty tick re-checks every subscription anyway.
- A transaction not visible in S commits after S, so after the consistent point, and is decoded whole: its messages and fence follow the normal path.
- One gap remains: a transaction whose commit record is below the consistent point but which is still in a synchronous-replication wait at S is neither decoded nor visible in S. So the feed runs a second all-dirty pass once every xid in progress in S (its `xip`) has ended.

Visibility replaces the `committed` flag and listener generations for this feed: the fence knows its xid before the commit.

### 7. Several nodes

Each node decodes for itself, with its own slot and walsender, as each listens today. The database decodes the WAL once per node, and each slot holds back `catalog_xmin`. `max_replication_slots` and `max_wal_senders` (10 by default) are shared with standbys and other CDC tools, which bounds the node count; the docs say so. One decoder fanning out to every node is an M5 question (observer ownership).

### 8. Large transactions

Commit-order delivery means a large transaction delays every transaction that commits after it until it is decoded: other callers' fences can pass `FenceTimeout`, and each node spills it separately. pgoutput's `streaming` option (protocol 2, PG 14+) sends a large transaction while it is in progress; the decoder then folds it into a table-dirty notification and discards it on abort. It is a follow-up task in M4, not part of the first logical feed.

### 9. Managed Postgres

| Provider | Setting | Role and connection |
|---|---|---|
| RDS / Aurora | `rds.logical_replication = 1` (reboot) | `rds_replication` |
| Cloud SQL | `cloudsql.logical_decoding = on` | `REPLICATION` |
| Supabase | on by default | `postgres`; `ReplicationDSN` is the direct connection, not Supavisor, which is IPv6-only without the IPv4 add-on |
| Neon | enable logical replication (restarts compute, cannot be turned off) | owner role; direct, non-pooled endpoint. A live replication connection probably keeps the compute from suspending (unverified) |

These are to be checked against each provider's current documentation when the docs task is done. The pgoutput `messages` option needs PG 14+; the feed requires PG 14+ in logical mode.

### 10. Moving a running app

Per node, at restart.

- **Data:** keep the triggers installed until every node runs the logical feed: notify-mode nodes need them, logical nodes ignore them. Then `UninstallTriggers`. The write-cost gain comes only after that. A notify-mode node refuses to start without the triggers (`checkTables` already does), so a rollback after `UninstallTriggers` fails loudly.
- **Fences** stay within a node's own feed.
- **Re-checks** must not be lost across modes: a lost one leaves a permission granted. While `Config.FeedMigrating` is set, every node sends `Revalidate*` both ways (NOTIFY and message), and logical-mode nodes also keep a LISTEN connection on `comet_reval`. A re-check received twice is harmless, since it only re-runs `fn`. On a logical node that LISTEN contributes re-checks and, after its own reconnect, `reval.all` only: the listener-generation fence release (`listened`, engine.go tick) is off in logical mode, since it would release a message fence before its writes are visible or its events arrive.
- **Events** are not sent both ways: nothing deduplicates them (`evtNote.ID` only defeats Postgres' own deduplication within a transaction), and a NOTIFY copy has no order relative to the decoded stream. During the rolling window, an event emitted on a node of one feed does not reach subscribers on nodes of the other. Events are at-most-once, and the docs say so.

### 11. Open source and hosted

Proposal, for the owner to decide: both feeds are in the open-source server. Logical decoding is what makes CometGo correct at scale and behind poolers, so leaving it out of OSS would ship a ceiling. The hosted offering's value is operating it: the database settings above, slot and lag monitoring, and a fan-out decoder for many nodes (M5).

## Review

A sparring agent reviewed the design from the code over three rounds, with experiments on Postgres 17, and ended at AGREE WITH CHANGES; the changes are applied above. What it forced:

- the visibility gate (§4): decoded is not visible yet; the gate lives in the decoder, holding commits in order, not in the tick,
- the reconnect rule by xid visibility instead of LSN (§6), after an experiment decoded a transaction that wrote before the consistent point and committed after it,
- a persistent slot resumed from the last fully received commit instead of a temporary slot per connection (§5), with zombie handling that never terminates another process's walsender, and a sweep limited to the app's own database,
- whole rows by default and the replica-identity hazard stated (§2), after an experiment showed a key-only column list, or a dropped primary key, breaks the app's own UPDATEs,
- transactional messages for Emit and Revalidate (§3), after an experiment showed a non-transactional message waits for the WAL writer,
- re-checks sent both ways during a migration, events not (§10).

## Tasks

Filed in Backlog with these dependencies (M4 module):

| Task | What | Depends on |
|---|---|---|
| CMTG-83 | Feed interface: the listener behind a feed abstraction (`Config.Feed`), notifications carry an xid. No behaviour change. | |
| CMTG-84 | `InstallPublication`, `UninstallTriggers`, and `checkTables` for logical mode (replica identity, partition root, key-only opt-in check) | |
| CMTG-85 | pgoutput decoder: grouping, coalescing, key encoding with a parity test against the triggers, messages, and the visibility gate with epoch-extended xids | CMTG-83 |
| CMTG-86 | Slot lifecycle: persistent slot, creation timeout, resume from the last received commit (verify the skip), zombie and conflict handling, `wal_sender_timeout`, confirming, liveness, missing slot, `SlotName`, health not-ready | CMTG-85 |
| CMTG-87 | Fences, events and re-checks as messages in logical mode; the recreated-slot rule by visibility in S and its second pass; the `listened` fence release off in logical mode | CMTG-85, CMTG-86 |
| CMTG-88 | Orphan slot sweeper and the slot headroom check | CMTG-86 |
| CMTG-89 | Migration: `FeedMigrating`, re-checks both ways, LISTEN on logical nodes for re-checks only | CMTG-87 |
| CMTG-90 | Streaming large transactions (pgoutput protocol 2) | CMTG-85 |
| CMTG-91 | Docs: logical mode in server.md, the managed providers (checked against their current docs), limitations, and §11.1 moved out of [later] | CMTG-87, CMTG-88 |
| CMTG-92 | Verify the exit criterion: feedbench at 16 and 64 writers and with wide rows, and the full suite with `Config.DB` through PgBouncer in transaction mode | CMTG-84, CMTG-87, CMTG-88, CMTG-89, CMTG-90 |
