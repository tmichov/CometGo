package cometgo

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Feed selects where the engine's change notifications come from
// (docs/design/m4-logical-feed.md §1).
type Feed int

const (
	// FeedNotify listens for the NOTIFYs sent by the triggers
	// InstallTriggers creates (PROTOCOL.md §11). The default.
	FeedNotify Feed = iota
	// FeedLogical decodes the write-ahead log through a logical replication
	// slot (PROTOCOL.md §11.1), over Config.ReplicationDSN, from the tables
	// InstallPublication publishes. PostgreSQL 14 or later.
	FeedLogical
)

// A feed delivers the §11 notification stream to engine.notify. Observers,
// fences and events handle its notifications the same whichever feed sends
// them.
type feed interface {
	// run maintains the feed until ctx ends.
	run(ctx context.Context)
	// fence runs the fence statement, the last of a method's transaction:
	// it signals token's fence once everything before it is delivered. It
	// reports false, sending nothing, for a transaction with no ID (it
	// wrote nothing), unless always.
	fence(ctx context.Context, tx pgx.Tx, token string, always bool) (bool, error)
	// commitFenced commits a method's transaction whose last statement
	// signalled token's fence, and marks the fence committed.
	commitFenced(ctx context.Context, tx pgx.Tx, token string) error
	// msgSQL is the statement sending an event (kind 'e') or a re-check
	// ('v') with payload $1, in the transaction it runs in.
	msgSQL(kind byte) string
	// restarted is called by tick, with the engine mutex held, for the
	// latest restart in a drain: notification listenN, of generation
	// listened (notification.listen). It releases the fences whose
	// notifications that restart lost.
	restarted(listened, listenN uint64)
}

// notifyFeed is the LISTEN/NOTIFY feed (engine.listen).
type notifyFeed struct{ e *engine }

func (f notifyFeed) run(ctx context.Context) { f.e.listen(ctx) }

func (f notifyFeed) fence(ctx context.Context, tx pgx.Tx, token string, always bool) (bool, error) {
	sql := "SELECT pg_notify('comet', $1) WHERE txid_current_if_assigned() IS NOT NULL"
	if always {
		sql = "SELECT pg_notify('comet', $1)"
	}
	tag, err := tx.Exec(ctx, sql, "1|f|"+token)
	return tag.RowsAffected() > 0, err
}

func (notifyFeed) msgSQL(kind byte) string {
	if kind == 'e' {
		return "SELECT pg_notify('" + evtChannel + "', $1)"
	}
	return "SELECT pg_notify('" + revalChannel + "', $1)"
}

// Postgres drops notifications for a channel nobody listens on, so the
// fence of a method that committed while the listener was down never
// arrives; the all-dirty tick of the next successful LISTEN releases it
// instead (restarted).
func (f notifyFeed) commitFenced(ctx context.Context, tx pgx.Tx, token string) error {
	f.e.listenMu.RLock()
	defer f.e.listenMu.RUnlock()
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	f.e.markCommitted(token)
	return nil
}

// A fence committed before listener generation `listened` issued LISTEN
// (listenMu) never reaches that connection: its notifications were either
// received earlier, and so are in this drain or an earlier one, or lost.
// The all-dirty notification of that LISTEN makes every observer stale
// from listenN, so the fence is released once they reflect it, and the
// fence's events, if received, go out before its res. A fence committed
// later is left to its own notification: releasing it here could send its
// res ahead of its events.
func (f notifyFeed) restarted(listened, listenN uint64) {
	for _, fc := range f.e.fences {
		if fc.committed && fc.gen < listened && fc.p == 0 {
			fc.p = listenN
		}
	}
}
