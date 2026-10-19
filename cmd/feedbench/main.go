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
//
// With -nodes N the logical modes run N Apps, one slot each, as N nodes of
// one app would, and also report what the slots cost the database: the
// walsenders' CPU, decoded and spilled bytes, retained WAL, and the
// visibility gates' snapshot queries (the M5 hub decision,
// docs/design/m5-hosted.md §8).
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// Logical modes only, over the measured window, summed over the nodes'
	// slots. CPU and the slot statistics are sampled apart, so each has its
	// own interval.
	cpu                float64 // the walsenders' CPU seconds, -1 if unavailable
	cpuTook, statsTook time.Duration
	decoded, spill     int64
	retained           int64 // the most WAL one slot held back, sampled about once a second
	snapshots          int64 // the gates' pg_current_snapshot() queries
}

// slotStats is one sample of the cometgo_feedbench* slots: the statistics
// read at at, and their walsenders' CPU seconds by PID read at cpuAt (nil if
// unavailable).
type slotStats struct {
	at, cpuAt                time.Time
	decoded, spill, retained int64
	cpu                      map[int32]float64
}

// slots are this database's feedbench slots.
const slots = `pg_replication_slots r WHERE r.slot_name LIKE 'cometgo\_feedbench%' AND r.database = current_database()`

func main() {
	dsn := flag.String("db", envOr("DATABASE_URL", "postgres://comet:comet@localhost:54329/comet"), "Postgres DSN")
	writers := flag.Int("writers", 16, "concurrent writing connections")
	warmup := flag.Duration("warmup", 3*time.Second, "writes before measuring, per mode")
	duration := flag.Duration("duration", 20*time.Second, "measured writes, per mode")
	only := flag.String("modes", "", "comma-separated modes to run (default all)")
	payload := flag.Int("payload", 100, "bytes in each row's text column; the logical modes decode whole rows, so wide rows cost them more")
	out := flag.String("out", "", "also write the Markdown report to this file")
	rounds := flag.Int("rounds", 1, "rounds of all modes, alternating; the report gives each mode's median and range (every transaction waits on a WAL flush, so one run is noisy)")
	nodes := flag.Int("nodes", 1, "logical modes: Apps on FeedLogical, one replication slot each, as the nodes of one app")
	procCmd := flag.String("proc", "", `logical modes: command that prints, in order, the files appended to it, run once per sample with the walsenders' /proc/<pid>/stat and cmdline, e.g. "docker compose exec -T postgres cat" (PIDs are the container's); empty reads this machine's /proc`)
	flag.Parse()
	if *nodes < 1 {
		log.Fatal("-nodes must be at least 1")
	}

	// Cancelling ends the run through its deferred shutdown, which drops the slots.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

	var walLevel, version, maxSlots, maxSenders string
	if err := pool.QueryRow(ctx, `SELECT current_setting('wal_level'), current_setting('server_version'),
		current_setting('max_replication_slots'), current_setting('max_wal_senders')`).Scan(&walLevel, &version, &maxSlots, &maxSenders); err != nil {
		log.Fatal(err)
	}
	// A slot left by a killed run holds WAL back and adds to what the next
	// run decodes.
	var active int
	if err := pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE r.active) FROM "+slots).Scan(&active); err != nil {
		log.Fatal(err)
	}
	if active > 0 {
		log.Fatalf("%d cometgo_feedbench slot(s) in use: another feedbench is running against this database", active)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_drop_replication_slot(r.slot_name) FROM "+slots+" AND NOT r.active"); err != nil {
		log.Fatalf("dropping leftover slots: %v", err)
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
			r, err := run(ctx, pool, *dsn, m, *writers, *payload, *nodes, *procCmd, *warmup, *duration)
			if err != nil {
				log.Fatalf("%s: %v", m.name, err)
			}
			log.Printf("round %d %-15s %8.0f tx/s  p50 %v  p99 %v", round+1, m.name, r.tps(), r.p50, r.p99)
			if m.logical {
				log.Printf("round %d %-15s walsender CPU %s, decoded %.1f MB/s, spilled %d B, retained %d B, %.0f snapshots/s",
					round+1, m.name, cores(r.cores()), mbps(r.decoded, r.statsTook), r.spill, r.retained, float64(r.snapshots)/r.took.Seconds())
			}
			results[i] = append(results[i], r)
		}
	}
	report := render(results, *rounds, *writers, *payload, *nodes, *duration, walLevel, version, maxSlots, maxSenders, *procCmd)
	fmt.Print(report)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func run(ctx context.Context, pool *pgxpool.Pool, dsn string, m mode, writers, payload, nodes int, procCmd string, warmup, duration time.Duration) (result, error) {
	if err := setup(ctx, pool, m, payload); err != nil {
		return result{}, err
	}
	feedCtx, stopFeed := context.WithCancel(ctx)
	var measuring atomic.Bool
	var feed sync.WaitGroup
	feedErr := make(chan error, 1)
	var snapshots atomic.Int64 // counted only while measuring
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
		feed.Go(func() { feedErr <- logicalFeed(feedCtx, dsn, nodes, &snapshots, &measuring, ready) })
		if err := <-ready; err != nil {
			stopFeed()
			return result{}, err
		}
	}
	defer func() {
		stopFeed()
		feed.Wait()
	}()

	var commits atomic.Int64
	var fenceN atomic.Int64
	lat := make([][]time.Duration, writers)
	loadCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Runs before the feed's shutdown: on an early return too.
	defer func() {
		stop()
		wg.Wait()
	}()
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
	if err := sleep(ctx, warmup); err != nil {
		return result{}, err
	}
	var before, after slotStats
	var err error
	if m.logical {
		if before, err = sample(ctx, pool, procCmd); err != nil {
			return result{}, err
		}
	}
	retained := before.retained
	winCtx, endWin := context.WithCancel(ctx)
	var sampler sync.WaitGroup
	var samplerErr error
	if m.logical {
		// Retained WAL is a sawtooth (the slot is confirmed about once a
		// second), so one point says little: the window's maximum.
		sampler.Go(func() {
			for sleep(winCtx, time.Second) == nil {
				var r int64
				if err := pool.QueryRow(winCtx, "SELECT coalesce(max(pg_wal_lsn_diff(pg_current_wal_lsn(), r.restart_lsn)), 0)::int8 FROM "+slots).Scan(&r); err != nil {
					if winCtx.Err() == nil {
						samplerErr = fmt.Errorf("retained WAL: %w", err)
					}
					return
				}
				retained = max(retained, r)
			}
		})
	}
	measuring.Store(true)
	begin := time.Now()
	err = sleep(ctx, duration)
	measuring.Store(false)
	took := time.Since(begin)
	endWin()
	sampler.Wait()
	if err = cmp.Or(err, samplerErr); err != nil {
		return result{}, err
	}
	if m.logical {
		if after, err = sample(ctx, pool, procCmd); err != nil {
			return result{}, err
		}
	}
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
	if m.logical {
		r.cpu = -1
		if c, ok := walsenderCPU(before.cpu, after.cpu, nodes); ok {
			r.cpu = c
		} else if before.cpu != nil && after.cpu != nil {
			log.Printf("%s: walsender CPU unavailable this round: PIDs %v, then %v, for %d node(s)",
				m.name, slices.Sorted(maps.Keys(before.cpu)), slices.Sorted(maps.Keys(after.cpu)), nodes)
		}
		r.cpuTook, r.statsTook = after.cpuAt.Sub(before.cpuAt), after.at.Sub(before.at)
		r.decoded, r.spill = after.decoded-before.decoded, after.spill-before.spill
		r.retained, r.snapshots = max(retained, after.retained), snapshots.Load()
	}
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

// logicalFeed runs n cometgo Apps on FeedLogical: the real streaming
// consumer, with its replication connection, visibility gate and engine,
// on slots cometgo_feedbench1..n. Shutdown drops the slots. Each gate
// snapshot query is added to snapshots while measuring.
func logicalFeed(ctx context.Context, dsn string, n int, snapshots *atomic.Int64, measuring *atomic.Bool, ready chan<- error) error {
	metrics := func(m cometgo.Metric) {
		if m.Name == cometgo.MetricFeedSnapshots && measuring.Load() {
			snapshots.Add(1)
		}
	}
	var apps []*cometgo.App
	var pools []*pgxpool.Pool
	// Every App shuts down (dropping its slot) before any pool closes.
	defer func() {
		for _, app := range apps {
			app.Shutdown(context.Background())
		}
		for _, pool := range pools {
			pool.Close()
		}
	}()
	for i := range n {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			ready <- err
			return err
		}
		pools = append(pools, pool)
		app := cometgo.New(cometgo.Config{DB: pool, Feed: cometgo.FeedLogical, ReplicationDSN: dsn,
			SlotName: fmt.Sprintf("feedbench%d", i+1), Metrics: metrics})
		if err := app.Start(context.Background()); err != nil {
			ready <- err
			return err
		}
		apps = append(apps, app)
	}
	deadline := time.Now().Add(time.Minute)
	for _, app := range apps {
		for app.Ready() != nil {
			if ctx.Err() != nil || time.Now().After(deadline) {
				err := fmt.Errorf("feed not ready: %w", cmp.Or(ctx.Err(), app.Ready()))
				ready <- err
				return err
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ready <- nil
	<-ctx.Done()
	return nil
}

// sample reads the feedbench slots' statistics: decoded and spilled bytes
// (pg_stat_replication_slots, PG 14+), the most WAL one holds back, and
// their walsenders' CPU seconds. Unreadable CPU is warned about once and
// left nil; a failed query is an error, since the run would mean nothing.
func sample(ctx context.Context, pool *pgxpool.Pool, procCmd string) (slotStats, error) {
	var st slotStats
	var pids []int32
	err := pool.QueryRow(ctx, `
		SELECT coalesce(sum(s.total_bytes), 0)::int8, coalesce(sum(s.spill_bytes), 0)::int8,
		       coalesce(max(pg_wal_lsn_diff(pg_current_wal_lsn(), r.restart_lsn)), 0)::int8,
		       coalesce(array_agg(r.active_pid) FILTER (WHERE r.active_pid IS NOT NULL), '{}')
		FROM pg_replication_slots r
		LEFT JOIN pg_stat_replication_slots s USING (slot_name)
		WHERE r.slot_name LIKE 'cometgo\_feedbench%' AND r.database = current_database()`).Scan(&st.decoded, &st.spill, &st.retained, &pids)
	if err != nil {
		return st, fmt.Errorf("slot stats: %w", err)
	}
	st.at = time.Now()
	cpu, err := procCPU(procCmd, pids)
	st.cpuAt = time.Now()
	if err != nil {
		warnCPU.Do(func() { log.Printf("walsender CPU unavailable (set -proc?): %v", err) })
		return st, nil
	}
	st.cpu = cpu
	return st, nil
}

var warnCPU sync.Once

// walsenderCPU is the CPU seconds the walsenders used between two samples;
// false unless both read the same nodes PIDs (a walsender that restarted
// mid-window would count from zero).
func walsenderCPU(before, after map[int32]float64, nodes int) (float64, bool) {
	if before == nil || after == nil || len(before) != nodes ||
		!maps.EqualFunc(before, after, func(float64, float64) bool { return true }) {
		return 0, false
	}
	var cpu float64
	for pid, c := range after {
		cpu += c - before[pid]
	}
	return cpu, true
}

// procCPU reads /proc/<pid>/stat and /proc/<pid>/cmdline of each pid,
// here or by one run of procCmd with every path, and returns each one's CPU
// seconds.
func procCPU(procCmd string, pids []int32) (map[int32]float64, error) {
	if len(pids) == 0 {
		return map[int32]float64{}, nil
	}
	var paths []string
	for _, pid := range pids {
		paths = append(paths, fmt.Sprintf("/proc/%d/stat", pid), fmt.Sprintf("/proc/%d/cmdline", pid))
	}
	var out []byte
	if procCmd == "" {
		for _, path := range paths {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	} else {
		args := append(strings.Fields(procCmd), paths...)
		var err error
		if out, err = exec.Command(args[0], args[1:]...).Output(); err != nil {
			return nil, fmt.Errorf("%s: %w", procCmd, err)
		}
	}
	return parseProc(string(out), pids)
}

// parseProc splits out, the stat line and cmdline of each of pids
// concatenated in order, and returns each one's CPU seconds; an error if
// one is malformed or not a walsender (without -proc against a container,
// its PIDs name this machine's unrelated processes). A stat line starts
// with its PID and ends in a newline; a cmdline holds no newline.
func parseProc(out string, pids []int32) (map[int32]float64, error) {
	cpu := map[int32]float64{}
	for i, pid := range pids {
		stat, rest, _ := strings.Cut(out, "\n")
		c, ok := parseStat(stat)
		if !ok || !strings.HasPrefix(stat, fmt.Sprintf("%d (", pid)) {
			return nil, fmt.Errorf("/proc/%d/stat: unexpected format %q", pid, stat)
		}
		cmdline := rest
		if i+1 < len(pids) {
			// rest runs on into the next stat line, whose "(" follows only its PID.
			line, _, _ := strings.Cut(rest, "\n")
			j := strings.LastIndex(line, fmt.Sprintf("%d (", pids[i+1]))
			if j < 0 {
				return nil, fmt.Errorf("/proc/%d/stat: not found in %q", pids[i+1], line)
			}
			cmdline, out = rest[:j], rest[j:]
		}
		if !strings.Contains(cmdline, "walsender") {
			return nil, fmt.Errorf("PID %d is not a walsender: %q", pid, cmdline)
		}
		cpu[pid] = c
	}
	return cpu, nil
}

// parseStat returns utime + stime, in seconds, from a /proc/<pid>/stat line.
func parseStat(stat string) (float64, bool) {
	// The command name (field 2) may hold spaces and parentheses, so count
	// from after its last ")": utime and stime are fields 14 and 15.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	// shortcut: assumes USER_HZ = 100, which Linux uses on every common architecture.
	return float64(utime+stime) / 100, true
}

// cores is the walsenders' CPU over the window, in cores; -1 if unknown.
func (r result) cores() float64 {
	if r.cpu < 0 {
		return -1
	}
	return r.cpu / r.cpuTook.Seconds()
}

// cpuMedian is the median of cores over the rounds where it was available.
func cpuMedian(rs []result) (float64, bool) {
	rs = slices.DeleteFunc(slices.Clone(rs), func(r result) bool { return r.cpu < 0 })
	if len(rs) == 0 {
		return 0, false
	}
	return median(rs, result.cores), true
}

func cores(c float64) string {
	if c < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f cores", c)
}

func mbps(b int64, d time.Duration) float64 { return float64(b) / 1e6 / d.Seconds() }

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

func render(results [][]result, rounds, writers, payload, nodes int, duration time.Duration, walLevel, version, maxSlots, maxSenders, procCmd string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# feedbench: change-feed cost on writes\n\n")
	fmt.Fprintf(&b, "Run %s.\n\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "- Postgres %s, wal_level=%s, on the same machine as the driver\n", version, walLevel)
	fmt.Fprintf(&b, "- Go %s, %s/%s, %d logical CPUs\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	fmt.Fprintf(&b, "- %d writers, %s measured per mode; each transaction updates one random row of %d (%d-byte text column) by primary key\n", writers, duration, rows, payload)
	fmt.Fprintf(&b, "- %d rounds, the modes alternating within each; tx/s, p50 and p99 are each mode's median over the rounds, and \"vs plain\" compares medians\n", rounds)
	var logical [][]result
	for _, rs := range results {
		if rs[0].mode.logical {
			logical = append(logical, rs)
		}
	}
	if len(logical) > 0 {
		fmt.Fprintf(&b, "- logical modes: %d node(s), one App and slot each; max_replication_slots=%s, max_wal_senders=%s\n", nodes, maxSlots, maxSenders)
	}
	b.WriteString("\n")
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
	if len(logical) == 0 {
		return b.String()
	}
	src := "this machine's /proc"
	if procCmd != "" {
		src = "`" + procCmd + "`"
	}
	fmt.Fprintf(&b, "\nWhat the %d slot(s) cost the database, per mode, medians over the rounds. Walsender CPU is user + system time of the slots' walsenders from %s, in cores (not available on managed Postgres); decoded and spilled bytes are from `pg_stat_replication_slots`; retained WAL is the most one slot held back, sampled about once a second over the window; snapshots are the visibility gates' `pg_current_snapshot()` queries (`feed_snapshots`).\n\n", nodes, src)
	fmt.Fprintf(&b, "| Mode | walsender CPU | decoded | spilled | retained WAL | gate snapshots |\n|---|---|---|---|---|---|\n")
	for _, rs := range logical {
		cpu := "n/a"
		if c, ok := cpuMedian(rs); ok {
			cpu = cores(c)
		}
		fmt.Fprintf(&b, "| %s | %s | %.1f MB/s | %d B | %d B | %.0f/s |\n", rs[0].mode.name, cpu,
			median(rs, func(r result) float64 { return mbps(r.decoded, r.statsTook) }),
			median(rs, func(r result) int64 { return r.spill }),
			median(rs, func(r result) int64 { return r.retained }),
			median(rs, func(r result) float64 { return float64(r.snapshots) / r.took.Seconds() }))
	}
	return b.String()
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
