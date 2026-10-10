package cometgo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// capIK is the hello/welcome capability for idempotency keys.
const capIK = "ik"

// maxIKLen bounds the idempotency key a client may send. A UUID is 36 bytes.
const maxIKLen = 128

// InstallIdempotency creates the comet_idempotency table that stores the
// results of calls carrying an idempotency key (PROTOCOL.md §5 ik). It is
// idempotent; run it at startup or from a migration. Apps whose clients never
// send ik do not need it.
//
// result is json rather than jsonb so a retry returns the exact bytes the
// first call returned.
func InstallIdempotency(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS comet_idempotency (
			ik         text NOT NULL,
			user_id    text NOT NULL,
			method     text NOT NULL,
			result     json,
			created_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (ik, user_id)
		);
		CREATE INDEX IF NOT EXISTS comet_idempotency_created_at ON comet_idempotency (created_at)`)
	return err
}

// claimIK runs at the start of a call's transaction. It returns the stored
// result of an earlier call with the same (ik, user), or nil when this call
// is the first and must run the handler.
//
// The first call inserts its row here and so holds the row lock until it
// commits or rolls back; a concurrent duplicate blocks on its own insert
// until then. If the first commits, the duplicate's insert conflicts and it
// reads the stored result; if the first rolls back (handler error), the
// duplicate's insert succeeds and it runs the handler itself.
func claimIK(ctx context.Context, tx pgx.Tx, ik, userID, method string) (stored json.RawMessage, err error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO comet_idempotency (ik, user_id, method) VALUES ($1, $2, $3)
		ON CONFLICT (ik, user_id) DO NOTHING`, ik, userID, method)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var storedMethod string
	var result []byte
	if err := tx.QueryRow(ctx, `
		SELECT method, result::text FROM comet_idempotency WHERE ik = $1 AND user_id = $2`,
		ik, userID).Scan(&storedMethod, &result); err != nil {
		// pgx.ErrNoRows: the row was deleted by TTL cleanup between the two
		// statements. Rare enough to surface as an error the client can retry.
		return nil, err
	}
	if storedMethod != method {
		return nil, NewError(CodeBadRequest, "idempotency key already used for method "+storedMethod)
	}
	return result, nil
}

// storeIK records the call's result in the row claimIK inserted, inside the
// same transaction as the handler's writes.
func storeIK(ctx context.Context, tx pgx.Tx, ik, userID string, val json.RawMessage) error {
	_, err := tx.Exec(ctx, `UPDATE comet_idempotency SET result = $3::text::json WHERE ik = $1 AND user_id = $2`,
		ik, userID, string(val))
	return err
}

// cleanIdempotency deletes stored results older than IdempotencyTTL, every
// cleanup interval until ctx is done. It does nothing while the table does
// not exist, so apps that never call InstallIdempotency see no errors.
func (a *App) cleanIdempotency(ctx context.Context) {
	t := time.NewTicker(min(a.cfg.IdempotencyTTL, time.Hour))
	defer t.Stop()
	for {
		if err := a.deleteExpiredIK(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("idempotency cleanup", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) deleteExpiredIK(ctx context.Context) error {
	var exists bool
	if err := a.db.QueryRow(ctx, `SELECT to_regclass('comet_idempotency') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return err
	}
	_, err := a.db.Exec(ctx, `DELETE FROM comet_idempotency WHERE created_at < now() - make_interval(secs => $1)`,
		a.cfg.IdempotencyTTL.Seconds())
	return err
}
