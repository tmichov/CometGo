// Command feedbench measures what the change feed costs a write workload:
// committed transactions per second on one table, with no feed, with the
// NOTIFY feed (InstallTriggers, plus a fence per transaction as a fenced call
// sends), and with the logical feed (InstallPublication and a cometgo App on
// FeedLogical streaming its slot, plus a fence as a logical message). It is
// the M4 measurement tool; see docs/bench/README.md.
//
// Every mode runs the same transaction: one UPDATE of a random row by
// primary key, from -writers concurrent connections. The logical modes need
// wal_level=logical, which make db sets.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmichov/cometgo"
)

const (
	schema = "feedbench"
	table  = schema + ".items"
	rows   = 10_000
)

type mode struct {
	name, desc string
	triggers   bool // InstallTriggers, with a LISTEN connection draining
	logical    bool // a publication on the table and a slot being read
	fence      string
}

var modes = []mode{
	{name: "plain", desc: "no feed"},
	{name: "triggers", desc: "InstallTriggers, one listener", triggers: true},
	{name: "triggers+fence", desc: "InstallTriggers and a NOTIFY fence per transaction", triggers: true,
		fence: "SELECT pg_notify('comet', '1|f|bench:' || $1::text)"},
	{name: "logical", desc: "InstallPublication (whole rows), an App on FeedLogical streaming its slot", logical: true},
	{name: "logical+fence", desc: "logical, and a transactional logical message per transaction", logical: true,
		fence: "SELECT pg_logical_emit_message(true, 'comet', 'f|bench:' || $1::text)"},
}

type result struct {
	mode     mode
	commits  int64
	took     time.Duration
	p50, p99 time.Duration
}

func main() {
	dsn := flag.String("db", envOr("DATABASE_URL", "postgres://comet:comet@localhost:54329/comet"), "Postgres DSN")
	writers := flag.Int("writers", 16, "concurrent writing connections")
	warmup := flag.Duration("warmup", 3*time.Second, "writes before measuring, per mode")
	duration := flag.Duration("duration", 20*time.Second, "measured writes, per mode")
	only := flag.String("modes", "", "comma-separated modes to run (default all)")
	payload := flag.Int("payload", 100, "bytes in each row's text column; the logical modes decode whole rows, so wide rows cost them more")
	out := flag.String("out", "", "also write the Markdown report to this file")
	rounds := flag.Int("rounds", 1, "rounds of all modes, alternating; the report gives each mode's median and range (every transaction waits on a WAL flush, so one run is noisy)")
	flag.Parse()

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(*dsn)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = int32(*writers + 2)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	var walLevel, version string
	if err := pool.QueryRow(ctx, "SELECT current_setting('wal_level'), current_setting('server_version')").Scan(&walLevel, &version); err != nil {
		log.Fatal(err)
	}
	var selected []mode
	for _, m := range modes {
		if *only != "" && !slices.Contains(strings.Split(*only, ","), m.name) {
			continue
		}
		if m.logical && walLevel != "logical" {
			log.Printf("skipping %s: wal_level is %s, not logical", m.name, walLevel)
			continue
		}
		selected = append(selected, m)
	}
	results := make([][]result, len(selected))
	for round := range *rounds {
		for i, m := range selected {
			r, err := run(ctx, pool, *dsn, m, *writers, *payload, *warmup, *duration)
			if err != nil {
				log.Fatalf("%s: %v", m.name, err)
			}
			log.Printf("round %d %-15s %8.0f tx/s  p50 %v  p99 %v", round+1, m.name, r.tps(), r.p50, r.p99)
			results[i] = append(results[i], r)
		}
	}
	report := render(results, *rounds, *writers, *payload, *duration, walLevel, version)
	fmt.Print(report)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func run(ctx context.Context, pool *pgxpool.Pool, dsn string, m mode, writers, payload int, warmup, duration time.Duration) (result, error) {
	if err := setup(ctx, pool, m, payload); err != nil {
		return result{}, err
	}
	feedCtx, stopFeed := context.WithCancel(ctx)
	var feed sync.WaitGroup
	feedErr := make(chan error, 1)
	switch {
	case m.triggers:
		ready := make(chan error, 1)
		feed.Go(func() { feedErr <- listen(feedCtx, dsn, ready) })
		if err := <-ready; err != nil {
			stopFeed()
			return result{}, err
		}
	case m.logical:
		ready := make(chan error, 1)
		feed.Go(func() { feedErr <- logicalFeed(feedCtx, dsn, ready) })
		if err := <-ready; err != nil {
			stopFeed()
			return result{}, err
		}
	}
	defer func() {
		stopFeed()
		feed.Wait()
	}()

	var measuring atomic.Bool
	var commits atomic.Int64
	var fenceN atomic.Int64
	lat := make([][]time.Duration, writers)
	loadCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Go(func() {
			for loadCtx.Err() == nil {
				start := time.Now()
				err := pgx.BeginFunc(loadCtx, pool, func(tx pgx.Tx) error {
					if _, err := tx.Exec(loadCtx, "UPDATE "+table+" SET n = n + 1 WHERE id = $1", rand.IntN(rows)+1); err != nil {
						return err
					}
					if m.fence != "" {
						_, err := tx.Exec(loadCtx, m.fence, strconv.FormatInt(fenceN.Add(1), 10))
						return err
					}
					return nil
				})
				if err != nil {
					if loadCtx.Err() == nil {
						errs <- err
						stop()
					}
					return
				}
				if measuring.Load() {
					commits.Add(1)
					lat[w] = append(lat[w], time.Since(start))
				}
			}
		})
	}
	time.Sleep(warmup)
	measuring.Store(true)
	begin := time.Now()
	time.Sleep(duration)
	measuring.Store(false)
	took := time.Since(begin)
	stop()
	wg.Wait()
	select {
	case err := <-errs:
		return result{}, err
	case err := <-feedErr:
		if err != nil && ctx.Err() == nil {
			return result{}, fmt.Errorf("feed: %w", err)
		}
	default:
	}
	all := slices.Concat(lat...)
	slices.Sort(all)
	r := result{mode: m, commits: commits.Load(), took: took}
	if len(all) > 0 {
		r.p50, r.p99 = all[len(all)/2], all[len(all)*99/100]
	}
	return r, nil
}

// setup recreates the schema and table, and the feed objects m needs.
func setup(ctx context.Context, pool *pgxpool.Pool, m mode, payload int) error {
	stmts := []string{
		"DROP PUBLICATION IF EXISTS feedbench",
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
		"CREATE SCHEMA " + schema,
		"CREATE TABLE " + table + " (id bigint PRIMARY KEY, n int NOT NULL DEFAULT 0, payload text NOT NULL)",
		fmt.Sprintf("INSERT INTO %s (id, payload) SELECT g, repeat('x', %d) FROM generate_series(1, %d) g", table, payload, rows),
		"VACUUM ANALYZE " + table,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	switch {
	case m.triggers:
		return cometgo.InstallTriggers(ctx, pool, table)
	case m.logical:
		// Whole rows, the feed's default (a key-only column list is opt-in).
		return cometgo.InstallPublication(ctx, pool, table)
	}
	return nil
}

// listen drains channel comet, as the server's listener does.
func listen(ctx context.Context, dsn string, ready chan<- error) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		ready <- err
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN comet"); err != nil {
		ready <- err
		return err
	}
	ready <- nil
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// logicalFeed runs a cometgo App on FeedLogical: the real streaming
// consumer, with its replication connection, visibility gate and engine,
// on slot cometgo_feedbench. Shutdown drops the slot.
func logicalFeed(ctx context.Context, dsn string, ready chan<- error) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		ready <- err
		return err
	}
	defer pool.Close()
	app := cometgo.New(cometgo.Config{DB: pool, Feed: cometgo.FeedLogical, ReplicationDSN: dsn, SlotName: "feedbench"})
	if err := app.Start(context.Background()); err != nil {
		ready <- err
		return err
	}
	defer app.Shutdown(context.Background())
	for deadline := time.Now().Add(time.Minute); app.Ready() != nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			err := fmt.Errorf("feed not ready: %w", app.Ready())
			ready <- err
			return err
		}
	}
	ready <- nil
	<-ctx.Done()
	return nil
}

func (r result) tps() float64 { return float64(r.commits) / r.took.Seconds() }

// median returns the middle value of f over rs (the upper one for an even
// count).
func median[T cmp.Ordered](rs []result, f func(result) T) T {
	vs := make([]T, len(rs))
	for i, r := range rs {
		vs[i] = f(r)
	}
	slices.Sort(vs)
	return vs[len(vs)/2]
}

func render(results [][]result, rounds, writers, payload int, duration time.Duration, walLevel, version string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# feedbench: change-feed cost on writes\n\n")
	fmt.Fprintf(&b, "Run %s.\n\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "- Postgres %s, wal_level=%s, on the same machine as the driver\n", version, walLevel)
	fmt.Fprintf(&b, "- Go %s, %s/%s, %d logical CPUs\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(&b, "- %d writers, %s measured per mode; each transaction updates one random row of %d (%d-byte text column) by primary key\n", writers, duration, rows, payload)
	fmt.Fprintf(&b, "- %d rounds, the modes alternating within each; tx/s, p50 and p99 are each mode's median over the rounds, and \"vs plain\" compares medians\n\n", rounds)
	fmt.Fprintf(&b, "| Mode | What runs | tx/s | range | vs plain | p50 | p99 |\n|---|---|---|---|---|---|---|\n")
	var plain float64
	for _, rs := range results {
		m := rs[0].mode
		tps := median(rs, result.tps)
		if m.name == "plain" {
			plain = tps
		}
		rel := "–"
		if plain > 0 {
			rel = fmt.Sprintf("%.0f%%", tps/plain*100)
		}
		lo, hi := slices.MinFunc(rs, func(a, b result) int { return cmp.Compare(a.tps(), b.tps()) }), slices.MaxFunc(rs, func(a, b result) int { return cmp.Compare(a.tps(), b.tps()) })
		fmt.Fprintf(&b, "| %s | %s | %.0f | %.0f–%.0f | %s | %s | %s |\n", m.name, m.desc, tps, lo.tps(), hi.tps(), rel,
			ms(median(rs, func(r result) time.Duration { return r.p50 })), ms(median(rs, func(r result) time.Duration { return r.p99 })))
	}
	return b.String()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
