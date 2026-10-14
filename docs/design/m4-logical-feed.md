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
- **No column list by default.** A key-only column list would keep the stream small, but from PG 15 an UPDATE or DELETE **fails** on a published table whose column list does not cover its replica identity. A later migration that changes the primary key, or sets `REPLICA IDENTITY FULL`, would then break the app's own writes; triggers can never do that. So the feed takes whole rows by default and ignores the non-key columns. A key-only list is an opt-in (`PublicationKeysOnly`), documented with that hazard, and `checkTables` then checks at start that each list still covers the table's replica identity.
- **Replica identity** `DEFAULT` (the primary key) is required, and checked at start. An UPDATE that changes the key carries the old key, so the old and new keys are reported, as today.
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

So **every tick checks that each xid it drained is visible in its snapshot** (`pg_current_snapshot()`, read once in the snapshot transaction and checked in Go). If one is not, the tick puts the drained notifications back, in order, and tries again shortly, as after a failed snapshot. Otherwise a fence could be released against a snapshot without its writes (breaking §6.2), and the observer would go clean on a stale result. Detached runs use the tick's exported snapshot, so the check covers them.

### 5. The replication connection and the slot

- `Config.ReplicationDSN`: a direct connection (`replication=database`); poolers do not carry the replication protocol. Required in logical mode. `Config.DB` may then go through a pooler.
- **A persistent slot per process**, `cometgo_<nodeID>` (the node ID is random per process). A dropped connection resumes the slot from the last confirmed LSN, so a network blip loses nothing and needs no all-dirty pass. Creating a slot waits for every transaction that holds an xid (2.14 s with one 2 s transaction in the spike, unbounded in general), so it happens once, at start, with a timeout and backoff; the node's feed is down until it succeeds, which the docs and a log line say.
- **No unbounded WAL.** The slot is dropped on clean shutdown. A crashed process leaves it behind: at start, each node drops `cometgo_*` slots that are inactive and older than `OrphanSlotAfter` (10 min; `inactive_since` on PG 17, two sightings that far apart before that). The docs require `max_slot_wal_keep_size` to bound what an orphan can hold meanwhile; a slot that loses its WAL is recreated, as at start.
- **Confirming.** The node confirms the commit LSN of each transaction once the engine has drained it, and, when idle, the WAL end from the server's keepalive, so writes to untracked tables do not pile up behind the slot.
- **Liveness.** The node sends a status update with `replyRequested` when it has heard nothing for `listenIdle`, and reconnects if no reply comes within `listenProbe`. Server keepalives alone come every `wal_sender_timeout / 2` (30 s), the same as `listenIdle`, which would cause spurious reconnects.

### 6. Starting the feed (the all-dirty tick)

At start, or after recreating a lost slot, changes committed before the slot is consistent are not decoded. Today `listenMu` makes fenced commits wait while `LISTEN` runs. Holding them while a slot is created is not acceptable, and an LSN rule does not work: a transaction that wrote before the consistent point but commits after it is decoded (verified: a message at 0/FB356DE8, consistent point 0/FB356E50, committed and decoded two seconds later).

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
- **Re-checks** must not be lost across modes: a lost one leaves a permission granted. While `Config.FeedMigrating` is set, every node sends `Revalidate*` both ways (NOTIFY and message), and logical-mode nodes also keep the LISTEN connection on `comet_reval`, with its `listenMu` and generation logic. A re-check received twice is harmless, since it only re-runs `fn`.
- **Events** are not sent both ways: nothing deduplicates them (`evtNote.ID` only defeats Postgres' own deduplication within a transaction), and a NOTIFY copy has no order relative to the decoded stream. During the rolling window, an event emitted on a node of one feed does not reach subscribers on nodes of the other. Events are at-most-once, and the docs say so.

### 11. Open source and hosted

Proposal, for the owner to decide: both feeds are in the open-source server. Logical decoding is what makes CometGo correct at scale and behind poolers, so leaving it out of OSS would ship a ceiling. The hosted offering's value is operating it: the database settings above, slot and lag monitoring, and a fan-out decoder for many nodes (M5).

## Tasks

To be filed after review, with dependencies and acceptance criteria.
