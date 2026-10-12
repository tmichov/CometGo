package cometgo

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InstallTriggers installs the change-feed triggers (PROTOCOL.md §11) on each
// table. It is idempotent; run it at startup or from a migration. Tables
// must have a primary key.
//
// Per table it creates statement-level AFTER triggers with transition tables,
// one per event (a transition-table trigger cannot list several events), and
// a TRUNCATE trigger. Each statement sends one notification on channel
// "comet": the changed primary keys, or a table-dirty marker when more than
// 1,000 rows changed or the key set does not fit in a payload.
func InstallTriggers(ctx context.Context, db *pgxpool.Pool, tables ...string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	for _, t := range tables {
		if err := installTable(ctx, tx, qualifyTable(t)); err != nil {
			return fmt.Errorf("cometgo: install triggers on %s: %w", t, err)
		}
	}
	return tx.Commit(ctx)
}

type pkCol struct {
	name, typ string
}

func installTable(ctx context.Context, tx pgx.Tx, table string) error {
	var schema, rel string
	err := tx.QueryRow(ctx, `
		SELECT n.nspname, c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, table).Scan(&schema, &rel)
	if err != nil {
		return fmt.Errorf("table not found: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = to_regclass($1) AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, table)
	if err != nil {
		return err
	}
	pk, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pkCol, error) {
		var c pkCol
		return c, r.Scan(&c.name, &c.typ)
	})
	if err != nil {
		return err
	}
	if len(pk) == 0 {
		return fmt.Errorf("no primary key")
	}

	qtable := pgx.Identifier{schema, rel}.Sanitize()
	wireName := schema + "." + rel
	prefix := triggerFuncPrefix(wireName)

	keyExpr := func(alias string) string {
		parts := make([]string, len(pk))
		for i, c := range pk {
			col := alias + "." + pgx.Identifier{c.name}.Sanitize()
			if c.typ == "bigint" {
				col += "::text" // int8 travels as a decimal string (§10)
			}
			parts[i] = col
		}
		return "jsonb_build_array(" + strings.Join(parts, ", ") + ")"
	}

	type event struct {
		suffix, op, on, referencing, countFrom, keys string
	}
	events := []event{
		{"ins", "I", "INSERT", "REFERENCING NEW TABLE AS new_rows", "new_rows",
			"SELECT DISTINCT " + keyExpr("r") + " FROM new_rows r"},
		{"upd", "U", "UPDATE", "REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows", "new_rows",
			// Union of old and new keys, so a primary-key change is seen.
			"SELECT " + keyExpr("r") + " FROM old_rows r UNION SELECT " + keyExpr("r") + " FROM new_rows r"},
		{"del", "D", "DELETE", "REFERENCING OLD TABLE AS old_rows", "old_rows",
			"SELECT DISTINCT " + keyExpr("r") + " FROM old_rows r"},
	}
	lit := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	fn := func(suffix string) string { return pgx.Identifier{schema, prefix + "_" + suffix}.Sanitize() }

	for _, ev := range events {
		rowsPrefix := "1|r|" + ev.op + "|" + wireName + "|"
		body := fmt.Sprintf(`
CREATE OR REPLACE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $cg$
DECLARE
  n bigint;
  payload text;
BEGIN
  SELECT count(*) INTO n FROM %[2]s;
  IF n = 0 THEN
    RETURN NULL;
  END IF;
  IF n <= 1000 THEN
    SELECT %[3]s || jsonb_agg(k)::text INTO payload FROM (%[4]s) s(k);
    IF octet_length(payload) <= 7500 THEN
      PERFORM pg_notify('comet', payload);
      RETURN NULL;
    END IF;
  END IF;
  PERFORM pg_notify('comet', %[5]s);
  RETURN NULL;
END
$cg$`, fn(ev.suffix), ev.countFrom, lit(rowsPrefix), ev.keys, lit("1|t|"+ev.op+"|"+wireName))
		if _, err := tx.Exec(ctx, body); err != nil {
			return err
		}
		trig := pgx.Identifier{"cometgo_" + ev.suffix}.Sanitize()
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, trig, qtable)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s AFTER %s ON %s %s FOR EACH STATEMENT EXECUTE FUNCTION %s()`,
			trig, ev.on, qtable, ev.referencing, fn(ev.suffix))); err != nil {
			return err
		}
	}

	truncBody := fmt.Sprintf(`
CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $cg$
BEGIN
  PERFORM pg_notify('comet', %s);
  RETURN NULL;
END
$cg$`, fn("trunc"), lit("1|t|T|"+wireName))
	if _, err := tx.Exec(ctx, truncBody); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS cometgo_trunc ON %s`, qtable)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER cometgo_trunc AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION %s()`,
		qtable, fn("trunc")))
	return err
}

// triggerFuncPrefix names the trigger functions for a table. The functions
// embed wireName in their payloads, so the name ties them to it.
func triggerFuncPrefix(wireName string) string {
	h := fnv.New32a()
	h.Write([]byte(wireName))
	return fmt.Sprintf("cometgo_%08x", h.Sum32())
}

// triggerSuffixes are the events installTable creates a trigger for, each
// named "cometgo_"+suffix and calling triggerFuncPrefix(wireName)+"_"+suffix.
var triggerSuffixes = []string{"ins", "upd", "del", "trunc"}

// checkTables fails if a table in any publication's Tables does not exist
// under exactly that name, or lacks an enabled trigger from InstallTriggers.
// Either would leave its subscriptions silently stale: the engine matches
// notifications to Tables by exact string. It runs once, at Start.
func (a *App) checkTables(ctx context.Context) error {
	seen := map[string]bool{}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(a.pubs)) {
		for _, t := range a.pubs[name].tables {
			if seen[t] {
				continue
			}
			seen[t] = true
			if err := a.checkTable(ctx, t); err != nil {
				errs = append(errs, fmt.Errorf("cometgo: publication %s: table %s: %w", name, t, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) checkTable(ctx context.Context, table string) error {
	var fns map[string]string // trigger name -> function name
	// Matched as the engine matches notifications: schema + "." + relname,
	// exactly, so a miscased or unqualified non-public name fails here too.
	// Replica-only ('R') and disabled ('D') triggers don't fire normally.
	err := a.db.QueryRow(ctx, `
		SELECT coalesce(jsonb_object_agg(t.tgname, p.proname) FILTER (WHERE t.tgname IS NOT NULL), '{}')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_trigger t ON t.tgrelid = c.oid AND t.tgname LIKE 'cometgo\_%' AND t.tgenabled IN ('O', 'A')
		LEFT JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname || '.' || c.relname = $1
		GROUP BY c.oid`, table).Scan(&fns)
	if errors.Is(err, pgx.ErrNoRows) {
		// to_regclass applies case folding and search_path, so it finds
		// what the name most likely meant. Tables entries without a schema
		// were qualified as public; resolve those through search_path.
		var meant *string
		if a.db.QueryRow(ctx, `
			SELECT n.nspname || '.' || c.relname
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.oid = to_regclass($1)`, strings.TrimPrefix(table, "public.")).Scan(&meant) == nil && meant != nil {
			return fmt.Errorf("not found; did you mean %q? Tables names must match the catalog exactly", *meant)
		}
		return errors.New("not found")
	}
	if err != nil {
		return err
	}
	var missing, stale []string
	for _, sfx := range triggerSuffixes {
		fn, ok := fns["cometgo_"+sfx]
		switch {
		case !ok:
			missing = append(missing, "cometgo_"+sfx)
		case fn != triggerFuncPrefix(table)+"_"+sfx:
			// Installed under another name (the table was renamed or moved
			// since), so its notifications carry that name.
			stale = append(stale, "cometgo_"+sfx)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing or disabled triggers %s; run cometgo.InstallTriggers for it", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		return fmt.Errorf("triggers %s were installed for another table name (renamed or moved since?); run cometgo.InstallTriggers for it", strings.Join(stale, ", "))
	}
	return nil
}
