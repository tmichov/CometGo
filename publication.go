package cometgo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pubName is the publication the logical feed reads (PROTOCOL.md §11.1).
const pubName = "cometgo"

// InstallPublication adds each table to the publication "cometgo" that the
// logical feed (FeedLogical) reads, creating it if missing, so the feed
// receives the table's whole rows. It is idempotent; run it at startup or
// from a migration, after the tables exist.
//
// The publication publishes inserts, updates, deletes and truncates, with
// publish_via_partition_root, so a partitioned table reports its changes
// under its own name. Postgres refuses UPDATE and DELETE on a published
// table without a usable replica identity, so each table, or each of its
// partitions, must have REPLICA IDENTITY DEFAULT with a primary key, or
// FULL. A later migration that drops the primary key makes those writes
// fail until the table has one again or leaves the publication.
func InstallPublication(ctx context.Context, db *pgxpool.Pool, tables ...string) error {
	return installPublication(ctx, db, false, tables)
}

// InstallPublicationKeysOnly is InstallPublication with each table's column
// list limited to its primary key, which keeps the change stream small for
// wide tables. It needs PostgreSQL 15+, and REPLICA IDENTITY DEFAULT: an
// UPDATE or DELETE fails on a published table whose column list does not
// cover its replica identity, so a migration that changes the primary key
// must run it again. Start checks the lists still cover it. Running
// InstallPublication for a table switches it back to whole rows.
func InstallPublicationKeysOnly(ctx context.Context, db *pgxpool.Pool, tables ...string) error {
	return installPublication(ctx, db, true, tables)
}

func installPublication(ctx context.Context, db *pgxpool.Pool, keysOnly bool, tables []string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if keysOnly {
		var v int
		if err := tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
			return err
		}
		if v < 150000 {
			return errors.New("cometgo: InstallPublicationKeysOnly needs PostgreSQL 15 or later")
		}
	}
	// Two processes installing at once would race to create the publication.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('cometgo_publication'))"); err != nil {
		return err
	}
	const opts = "publish = 'insert, update, delete, truncate', publish_via_partition_root = true"
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_publication WHERE pubname = $1)", pubName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		_, err = tx.Exec(ctx, "ALTER PUBLICATION "+pubName+" SET ("+opts+")")
	} else {
		_, err = tx.Exec(ctx, "CREATE PUBLICATION "+pubName+" WITH ("+opts+")")
	}
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := publishTable(ctx, tx, qualifyTable(t), keysOnly); err != nil {
			return fmt.Errorf("cometgo: install publication for %s: %w", t, err)
		}
	}
	return tx.Commit(ctx)
}

func publishTable(ctx context.Context, tx pgx.Tx, table string, keysOnly bool) error {
	var schema, rel string
	var oid uint32
	err := tx.QueryRow(ctx, `
		SELECT c.oid, n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, table).Scan(&oid, &schema, &rel)
	if err != nil {
		return fmt.Errorf("table not found: %w", err)
	}
	// Publishing a table Postgres cannot replicate UPDATEs of would make the
	// app's own writes fail from the commit on.
	leaves, err := leafIdentities(ctx, tx, oid)
	if err != nil {
		return err
	}
	if err := identityProblem(leaves, keysOnly); err != nil {
		return err
	}
	var want []string // the column list; nil for whole rows
	if keysOnly {
		if err := tx.QueryRow(ctx, `
			SELECT array_agg(a.attname ORDER BY a.attnum)
			FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
			WHERE i.indrelid = $1 AND i.indisprimary`, oid).Scan(&want); err != nil {
			return err
		}
		if len(want) == 0 {
			return errors.New("no primary key")
		}
	}
	// A member's column list, from pg_publication_tables.attnames (PG 15+;
	// to_jsonb leaves it null before). With no list it names every column.
	var member bool
	var have, all []string
	err = tx.QueryRow(ctx, `
		SELECT true,
			ARRAY(SELECT jsonb_array_elements_text(coalesce(to_jsonb(pt)->'attnames', '[]'))),
			ARRAY(SELECT a.attname FROM pg_attribute a WHERE a.attrelid = $3 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum)
		FROM pg_publication_tables pt
		WHERE pt.pubname = $1 AND pt.schemaname = $2 AND pt.tablename = $4`, pubName, schema, oid, rel).Scan(&member, &have, &all)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	qtable := pgx.Identifier{schema, rel}.Sanitize()
	if member {
		wholeRows := len(have) == 0 || slices.Equal(have, all)
		if keysOnly && slices.Equal(have, want) || !keysOnly && wholeRows {
			return nil
		}
		// Swapped in this transaction, so the feed never sees the table
		// leave the publication.
		if _, err := tx.Exec(ctx, "ALTER PUBLICATION "+pubName+" DROP TABLE "+qtable); err != nil {
			return err
		}
	}
	add := "ALTER PUBLICATION " + pubName + " ADD TABLE " + qtable
	if keysOnly {
		cols := make([]string, len(want))
		for i, c := range want {
			cols[i] = pgx.Identifier{c}.Sanitize()
		}
		add += " (" + strings.Join(cols, ", ") + ")"
	}
	_, err = tx.Exec(ctx, add)
	return err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// leafIdentity is the replica identity of a table, or of one leaf
// partition of a partitioned table: Postgres checks it on the leaf a row
// is written to.
type leafIdentity struct {
	name  string
	ident string   // pg_class.relreplident: d(efault), f(ull), i(ndex), n(othing)
	cols  []string // the identity's columns: the primary key's for d, all for f
}

func leafIdentities(ctx context.Context, q querier, oid uint32) ([]leafIdentity, error) {
	rows, err := q.Query(ctx, `
		SELECT c.oid::regclass::text, c.relreplident::text,
			coalesce(ARRAY(
				SELECT a.attname FROM pg_attribute a
				WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
					AND (c.relreplident = 'f' OR a.attnum = ANY(
						(SELECT i.indkey FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)::int2[]))
				ORDER BY a.attnum), '{}')
		FROM pg_class c
		-- pg_partition_tree is empty for a table that is not partitioned.
		WHERE c.oid IN (SELECT relid FROM pg_partition_tree($1::oid::regclass) WHERE isleaf)
			OR c.oid = $1 AND c.relkind = 'r'
		ORDER BY 1`, oid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (leafIdentity, error) {
		var l leafIdentity
		return l, r.Scan(&l.name, &l.ident, &l.cols)
	})
}

// identityProblem reports a leaf whose UPDATEs and DELETEs Postgres would
// refuse once published, or which the feed cannot key: it needs REPLICA
// IDENTITY DEFAULT with a primary key, or FULL (and DEFAULT for a
// key-only column list, which cannot cover FULL's every column).
func identityProblem(leaves []leafIdentity, keysOnly bool) error {
	for _, l := range leaves {
		switch {
		case l.ident == "d" && len(l.cols) == 0:
			return fmt.Errorf("%s has no primary key; add one, or set REPLICA IDENTITY FULL", l.name)
		case l.ident == "f" && keysOnly:
			return fmt.Errorf("%s has REPLICA IDENTITY FULL, which a key-only column list cannot cover; use whole rows (InstallPublication)", l.name)
		case l.ident != "d" && l.ident != "f":
			return fmt.Errorf("%s has REPLICA IDENTITY %s; set DEFAULT (with a primary key) or FULL", l.name, map[string]string{"i": "USING INDEX", "n": "NOTHING"}[l.ident])
		}
	}
	return nil
}

// checkPublished is checkTable for the logical feed: the table must be in
// the publication, under its own name, with a replica identity Postgres
// can replicate UPDATEs by and, under a column list, that list covering it.
func (a *App) checkPublished(ctx context.Context, table string, oid uint32) error {
	var cols []string
	var listed bool
	err := a.db.QueryRow(ctx, `
		SELECT coalesce(to_jsonb(pt)->'attnames' IS NOT NULL, false),
			ARRAY(SELECT jsonb_array_elements_text(coalesce(to_jsonb(pt)->'attnames', '[]')))
		FROM pg_publication_tables pt
		WHERE pt.pubname = $1 AND pt.schemaname || '.' || pt.tablename = $2`, pubName, table).Scan(&listed, &cols)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("not in publication %q; run cometgo.InstallPublication for it", pubName)
	}
	if err != nil {
		return err
	}
	leaves, err := leafIdentities(ctx, a.db, oid)
	if err != nil {
		return err
	}
	if err := identityProblem(leaves, false); err != nil {
		return err
	}
	if !listed {
		return nil
	}
	// The list is the root's (publish_via_partition_root) and Postgres
	// checks it against each leaf's identity.
	for _, l := range leaves {
		var missing []string
		for _, c := range l.cols {
			if !slices.Contains(cols, c) {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("the publication's column list does not cover the replica identity of %s (missing %s), so its UPDATEs and DELETEs fail; run cometgo.InstallPublicationKeysOnly or cometgo.InstallPublication for it", l.name, strings.Join(missing, ", "))
		}
	}
	return nil
}

// checkPublication checks the publication's own settings: a feed that
// misses an action, or reports partitions under their own names, would
// leave subscriptions silently stale.
func (a *App) checkPublication(ctx context.Context) error {
	var ok bool
	err := a.db.QueryRow(ctx, `
		SELECT pubinsert AND pubupdate AND pubdelete AND pubtruncate AND pubviaroot
		FROM pg_publication WHERE pubname = $1`, pubName).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("cometgo: publication %q not found; run cometgo.InstallPublication", pubName)
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cometgo: publication %q must publish insert, update, delete and truncate via the partition root; run cometgo.InstallPublication", pubName)
	}
	return nil
}
