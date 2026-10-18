package cometgo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

// A crashed process leaves its slot behind, holding WAL and catalog_xmin.
// Every node sweeps the cometgo_* slots of its own database (slots are
// cluster-wide, and another app or environment may share the cluster) that
// have been inactive for Config.OrphanSlotAfter (design §5).

type seenSlot struct {
	lsn   pglogrepl.LSN
	since time.Time
}

// sweepEvery is how often the sweeper looks: often enough that an orphan
// goes within about 1.5 OrphanSlotAfter, and at most once a minute.
func sweepEvery(after time.Duration) time.Duration {
	return min(after/2, time.Minute)
}

func (f *logicalFeed) sweepLoop(ctx context.Context) {
	after := f.e.a.cfg.OrphanSlotAfter
	if after < 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepEvery(after)):
		}
		if err := f.sweep(ctx, 0); err != nil && ctx.Err() == nil {
			f.e.a.log.Warn("logical feed: orphan slot sweep failed", "err", err)
		}
	}
}

// sweep drops this database's cometgo_* slots, other than ours, that have
// been inactive for OrphanSlotAfter. PG 17 records inactive_since, and its
// age is taken on the database's clock, so a node whose clock is off by
// more than OrphanSlotAfter cannot drop a live node's slot mid-reconnect.
// Before 17, a slot counts from the first sweep that saw it inactive with
// its current confirmed_flush_lsn, on this node's clock (it never compares
// with another clock), and starts again whenever it is seen active or with
// another LSN. Dropping a slot that became active in between fails, and is
// left alone. elapsed is added to both ages, so tests need not wait.
func (f *logicalFeed) sweep(ctx context.Context, elapsed time.Duration) error {
	after := f.e.a.cfg.OrphanSlotAfter
	now := f.clock().Add(elapsed)
	rows, err := f.e.a.db.Query(ctx, `
		SELECT slot_name, active, coalesce(confirmed_flush_lsn, '0/0'),
			extract(epoch FROM now() - (to_jsonb(s)->>'inactive_since')::timestamptz)
		FROM pg_replication_slots s
		WHERE database = current_database() AND slot_type = 'logical'
			AND slot_name LIKE 'cometgo\_%' AND slot_name <> $1`, f.slot)
	if err != nil {
		return err
	}
	type slotRow struct {
		name     string
		active   bool
		lsn      pglogrepl.LSN
		inactive *float64 // seconds since inactive_since, on the database's clock (PG 17)
	}
	slots, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (s slotRow, err error) {
		return s, r.Scan(&s.name, &s.active, &s.lsn, &s.inactive)
	})
	if err != nil {
		return err
	}
	seen := map[string]seenSlot{}
	for _, s := range slots {
		var age time.Duration
		switch {
		case s.active:
			continue
		case s.inactive != nil && !f.noInactiveSince:
			age = time.Duration(*s.inactive*float64(time.Second)) + elapsed
		default:
			prev, ok := f.swept[s.name]
			if !ok || prev.lsn != s.lsn {
				prev = seenSlot{lsn: s.lsn, since: now}
			}
			seen[s.name] = prev
			age = now.Sub(prev.since)
		}
		if age < after {
			continue
		}
		if _, err := f.e.a.db.Exec(ctx, "SELECT pg_drop_replication_slot($1)", s.name); err != nil {
			f.e.a.log.Warn("logical feed: orphan slot not dropped", "slot", s.name, "err", err)
			continue
		}
		delete(seen, s.name)
		f.e.a.log.Info("logical feed: dropped orphan slot", "slot", s.name, "inactive_for", age.Round(time.Second))
	}
	f.swept = seen
	return nil
}

// checkHeadroom fails, before creating our slot, when max_replication_slots
// leaves no room for it, naming the cometgo_* slots that hold places.
func (f *logicalFeed) checkHeadroom(ctx context.Context) error {
	var used, limit int
	var ours []string
	err := f.e.a.db.QueryRow(ctx, `
		SELECT count(*), current_setting('max_replication_slots')::int,
			coalesce(array_agg(slot_name || ' (' || coalesce(database, 'physical') || CASE WHEN active THEN ', active)' ELSE ', inactive)' END
				ORDER BY slot_name) FILTER (WHERE slot_name LIKE 'cometgo\_%'), '{}')
		FROM pg_replication_slots`).Scan(&used, &limit, &ours)
	if err != nil {
		return err
	}
	if used < limit {
		return nil
	}
	held := "none"
	if len(ours) > 0 {
		held = strings.Join(ours, ", ")
	}
	return fmt.Errorf("no room for slot %s: all %d of max_replication_slots are in use; cometgo slots among them: %s. Raise max_replication_slots, or drop the slots of nodes that are gone (the sweeper drops this database's after Config.OrphanSlotAfter)", f.slot, limit, held)
}
