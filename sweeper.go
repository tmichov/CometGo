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
		if err := f.sweep(ctx, time.Now()); err != nil && ctx.Err() == nil {
			f.e.a.log.Warn("logical feed: orphan slot sweep failed", "err", err)
		}
	}
}

// sweep drops this database's cometgo_* slots, other than ours, that have
// been inactive for OrphanSlotAfter as of now. PG 17 records inactive_since;
// before, a slot counts from the first sweep that saw it inactive with its
// current confirmed_flush_lsn, and starts again whenever it is seen active
// or with another LSN. Dropping a slot that became active in between fails,
// and is left alone.
func (f *logicalFeed) sweep(ctx context.Context, now time.Time) error {
	after := f.e.a.cfg.OrphanSlotAfter
	rows, err := f.e.a.db.Query(ctx, `
		SELECT slot_name, active, coalesce(confirmed_flush_lsn, '0/0'),
			(to_jsonb(s)->>'inactive_since')::timestamptz
		FROM pg_replication_slots s
		WHERE database = current_database() AND slot_type = 'logical'
			AND slot_name LIKE 'cometgo\_%' AND slot_name <> $1`, f.slot)
	if err != nil {
		return err
	}
	type slotRow struct {
		name          string
		active        bool
		lsn           pglogrepl.LSN
		inactiveSince *time.Time
	}
	slots, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (s slotRow, err error) {
		return s, r.Scan(&s.name, &s.active, &s.lsn, &s.inactiveSince)
	})
	if err != nil {
		return err
	}
	seen := map[string]seenSlot{}
	for _, s := range slots {
		var since time.Time
		switch {
		case s.active:
			continue
		case s.inactiveSince != nil && !f.noInactiveSince:
			since = *s.inactiveSince
		default:
			prev, ok := f.swept[s.name]
			if !ok || prev.lsn != s.lsn {
				prev = seenSlot{lsn: s.lsn, since: now}
			}
			seen[s.name] = prev
			since = prev.since
		}
		if now.Sub(since) < after {
			continue
		}
		if _, err := f.e.a.db.Exec(ctx, "SELECT pg_drop_replication_slot($1)", s.name); err != nil {
			f.e.a.log.Warn("logical feed: orphan slot not dropped", "slot", s.name, "err", err)
			continue
		}
		delete(seen, s.name)
		f.e.a.log.Info("logical feed: dropped orphan slot", "slot", s.name, "inactive_since", since)
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
