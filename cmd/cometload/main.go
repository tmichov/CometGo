// Command cometload drives a CometGo server with many WebSocket clients and
// reports call latency and engine health. It is the M3 baseline harness; see
// docs/bench/README.md.
//
// The driver starts the server under test as a child process (itself, with
// -serve) so the server's RSS is measured without the clients', then opens
// the clients, writes to Postgres at a fixed rate and makes calls at a fixed
// rate.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmichov/cometgo"
)

type params struct {
	dsn          string
	clients      int
	subs         int
	sharedRatio  float64
	users        int
	groups       int
	rows         int
	writes       int
	calls        int
	warmup       time.Duration
	duration     time.Duration
	scenario     string
	slowObserver time.Duration
	slowFn       time.Duration
	out          string
	pgContainer  string
}

// sharedSubs is how many of each client's subs are shared publications; the
// rest are per-user.
func (p params) sharedSubs() int { return int(float64(p.subs)*p.sharedRatio + 0.5) }
func (p params) userSubs() int   { return p.subs - p.sharedSubs() }

func main() {
	var p params
	serveMode := flag.Bool("serve", false, "internal: run the server under test")
	flag.StringVar(&p.dsn, "db", envOr("DATABASE_URL", "postgres://comet:comet@localhost:54329/comet"), "Postgres DSN")
	flag.IntVar(&p.clients, "clients", 1000, "WebSocket clients")
	flag.IntVar(&p.subs, "subs", 5, "subscriptions per client")
	flag.Float64Var(&p.sharedRatio, "shared", 0.4, "fraction of each client's subs that are shared publications")
	flag.IntVar(&p.users, "users", 500, "distinct users; clients are spread over them round-robin")
	flag.IntVar(&p.groups, "groups", 50, "shared groups (one shared observer each)")
	flag.IntVar(&p.rows, "rows", 20, "rows per shared group and per user bucket")
	flag.IntVar(&p.writes, "writes", 200, "direct Postgres writes per second, outside any call")
	flag.IntVar(&p.calls, "calls", 200, "method calls per second, spread over random clients")
	flag.DurationVar(&p.warmup, "warmup", 10*time.Second, "load before measuring")
	flag.DurationVar(&p.duration, "duration", 60*time.Second, "measured load")
	flag.StringVar(&p.scenario, "scenario", "base", `"base", or "slow" to add one slow observer and one slow fn`)
	flag.DurationVar(&p.slowObserver, "slow-observer", 500*time.Millisecond, "slow scenario: pg_sleep in the slow observer, re-run on every shared write")
	flag.DurationVar(&p.slowFn, "slow-fn", 2*time.Second, "slow scenario: pg_sleep in the slow fn, called back to back by one client")
	flag.StringVar(&p.out, "out", "", "also write the Markdown report to this file")
	flag.StringVar(&p.pgContainer, "pg-container", "", `Postgres container for DB CPU (default: "docker compose ps -q postgres")`)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *serveMode {
		if err := serve(ctx, p); err != nil {
			log.Fatal(err)
		}
		return
	}
	if p.scenario != "base" && p.scenario != "slow" {
		log.Fatalf("unknown -scenario %q", p.scenario)
	}
	if p.subs < 1 || p.clients < 1 || p.users < 1 || p.groups < 1 || p.rows < 1 {
		log.Fatal("-clients, -subs, -users, -groups and -rows must be at least 1")
	}
	rep, err := drive(ctx, p)
	if err != nil {
		log.Fatal(err)
	}
	md := rep.markdown()
	fmt.Print(md)
	if p.out != "" {
		if err := os.WriteFile(p.out, []byte(md), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// callStats aggregates call results sent during the measurement window.
type callStats struct {
	mu        sync.Mutex
	measuring bool
	latency   map[string][]float64 // seconds, per method, ok calls only
	lag       map[string]int
	errs      map[string]int // by error code
	sent      int
	answered  int
}

func (s *callStats) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.measuring = true
	s.latency, s.lag, s.errs = map[string][]float64{}, map[string]int{}, map[string]int{}
	s.sent, s.answered = 0, 0
}

type pending struct {
	method   string
	start    time.Time
	measured bool
}

type client struct {
	ws      *websocket.Conn
	user    string
	stats   *callStats
	mu      sync.Mutex
	nextID  int64
	pending map[int64]pending
	ready   chan struct{} // closed once every sub is ready or ended
	ended   atomic.Int32  // subs the server ended
	closed  atomic.Bool
}

// inMsg is the part of a server message the driver reads.
type inMsg struct {
	T   string `json:"t"`
	ID  int64  `json:"id"`
	OK  bool   `json:"ok"`
	Lag bool   `json:"lag"`
	Err *struct {
		Code string `json:"code"`
	} `json:"err"`
	Subs []struct {
		S     int             `json:"s"`
		Ready bool            `json:"ready"`
		End   json.RawMessage `json:"end"`
	} `json:"subs"`
}

func dial(ctx context.Context, url, user string, subs []map[string]any, stats *callStats) (*client, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{Subprotocols: []string{cometgo.Subprotocol}})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(4 << 20)
	c := &client{ws: ws, user: user, stats: stats, pending: map[int64]pending{}, ready: make(chan struct{})}
	if err := c.send(ctx, map[string]any{"t": "hello", "v": 1, "auth": map[string]any{"token": user}}); err != nil {
		return nil, err
	}
	for i, s := range subs {
		s["t"], s["s"] = "sub", i+1
		if err := c.send(ctx, s); err != nil {
			return nil, err
		}
	}
	go c.read(ctx, len(subs))
	return c, nil
}

func (c *client) send(ctx context.Context, m map[string]any) error {
	b, _ := json.Marshal(m)
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func (c *client) read(ctx context.Context, nsubs int) {
	defer c.closed.Store(true)
	waiting := map[int]bool{}
	for s := 1; s <= nsubs; s++ {
		waiting[s] = true
	}
	for {
		_, b, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		var m inMsg
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		switch m.T {
		case "tx":
			for _, e := range m.Subs {
				if len(e.End) > 0 {
					c.ended.Add(1)
				}
				if waiting[e.S] && (e.Ready || len(e.End) > 0) {
					delete(waiting, e.S)
					if len(waiting) == 0 {
						close(c.ready)
					}
				}
			}
		case "res":
			c.mu.Lock()
			p, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok && p.measured {
				c.stats.result(p, m, time.Since(p.start))
			}
		}
	}
}

func (s *callStats) result(p pending, m inMsg, took time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answered++
	if !m.OK {
		code := "?"
		if m.Err != nil {
			code = m.Err.Code
		}
		s.errs[p.method+" "+code]++
		return
	}
	s.latency[p.method] = append(s.latency[p.method], took.Seconds())
	if m.Lag {
		s.lag[p.method]++
	}
}

// call sends a call without waiting for its result. The write gets its own
// timeout rather than the load's context: coder/websocket closes the socket
// when a write's context is cancelled, which would turn the end of the load
// into dropped clients and lost calls.
func (c *client) call(method string, args any) error {
	c.stats.mu.Lock()
	measured := c.stats.measuring
	if measured {
		c.stats.sent++
	}
	c.stats.mu.Unlock()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.pending[id] = pending{method: method, start: time.Now(), measured: measured}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.send(ctx, map[string]any{"t": "call", "id": id, "m": method, "args": args})
	if err != nil {
		// Never sent, so it must not count as sent or wait for a res.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if measured {
			c.stats.mu.Lock()
			c.stats.sent--
			c.stats.mu.Unlock()
		}
	}
	return err
}

func (c *client) inflight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// subsFor returns client i's subs: shared groups first, then user buckets.
func subsFor(p params, i int) []map[string]any {
	var subs []map[string]any
	for k := range p.sharedSubs() {
		subs = append(subs, map[string]any{"name": "shared.group", "args": map[string]any{"g": (i*p.sharedSubs() + k) % p.groups}})
	}
	for b := range p.userSubs() {
		subs = append(subs, map[string]any{"name": "user.bucket", "args": map[string]any{"b": b}})
	}
	return subs
}

func drive(ctx context.Context, p params) (*report, error) {
	rep := &report{p: p, started: time.Now()}
	pool, err := pgxpool.New(ctx, p.dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&rep.pgVersion); err != nil {
		return nil, fmt.Errorf("database (is `make db` running?): %w", err)
	}
	if err := setupSchema(ctx, pool, p); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}

	srv, err := startServer(ctx, p)
	if err != nil {
		return nil, err
	}
	defer srv.stop()
	time.Sleep(2 * time.Second) // let the listener connect and the heap settle
	rep.rssIdle = rss(srv.cmd.Process.Pid)

	stats := &callStats{}
	url := "ws://" + srv.addr + "/_comet"
	clients := make([]*client, p.clients)
	connectStart := time.Now()
	errc := make(chan error, p.clients)
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := dial(ctx, url, fmt.Sprintf("u%d", i%p.users), subsFor(p, i), stats)
			if err != nil {
				errc <- err
				return
			}
			clients[i] = c
		}()
	}
	wg.Wait()
	close(errc)
	if err := <-errc; err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	timeout := time.After(2 * time.Minute)
	for _, c := range clients {
		select {
		case <-c.ready:
		case <-timeout:
			return nil, errors.New("subs not ready after 2m")
		}
	}
	rep.connectTook = time.Since(connectStart)
	for _, c := range clients {
		rep.subsEnded += int(c.ended.Load())
	}
	time.Sleep(2 * time.Second)
	rep.rssLoaded = rss(srv.cmd.Process.Pid)

	var slowClient *client
	if p.scenario == "slow" {
		// Its own callStats: slow.fn must not count with the other calls.
		slowClient, err = dial(ctx, url, "slow", []map[string]any{{"name": "slow.observer", "args": map[string]any{}}}, &callStats{})
		if err != nil {
			return nil, fmt.Errorf("slow client: %w", err)
		}
	}

	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	var load sync.WaitGroup
	var writesDone, writesSkipped, callsSkipped atomic.Int64
	load.Go(func() { writeLoad(loadCtx, pool, p, &writesDone, &writesSkipped) })
	load.Go(func() { callLoad(loadCtx, clients, p, &callsSkipped) })
	slowFns := &slowFnStats{}
	if slowClient != nil {
		load.Go(func() { slowFnLoad(loadCtx, slowClient, slowFns) })
	}

	select {
	case <-time.After(p.warmup):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if _, err := srv.stats(true); err != nil {
		return nil, err
	}
	cpu0, cpuErr := dbCPU(p.pgContainer)
	writes0, writesSkipped0, callsSkipped0 := writesDone.Load(), writesSkipped.Load(), callsSkipped.Load()
	stats.begin()
	slowFns.begin()
	measureStart := time.Now()

	select {
	case <-time.After(p.duration):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	rep.measured = time.Since(measureStart)
	cpu1, cpuErr1 := dbCPU(p.pgContainer)
	if cpuErr == nil && cpuErr1 == nil {
		rep.dbCPU = float64(cpu1-cpu0) / float64(rep.measured.Microseconds()) * 100
	} else {
		rep.dbCPUErr = errors.Join(cpuErr, cpuErr1).Error()
	}
	if rep.server, err = srv.stats(false); err != nil {
		return nil, err
	}
	rep.writes = writesDone.Load() - writes0
	rep.writesSkipped = writesSkipped.Load() - writesSkipped0
	rep.callsSkipped = callsSkipped.Load() - callsSkipped0
	rep.slowFn = slowFns.snapshot()
	stopLoad()
	load.Wait()

	// Calls sent in the window but not answered yet still count, as lost if
	// they never come back.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		stats.mu.Lock()
		done := stats.answered >= stats.sent
		stats.mu.Unlock()
		if done {
			break
		}
	}
	stats.mu.Lock()
	rep.calls = stats
	rep.callsSent, rep.callsAnswered = stats.sent, stats.answered
	stats.measuring = false
	stats.mu.Unlock()
	for _, c := range clients {
		if c.closed.Load() {
			rep.dropped++
		}
	}
	return rep, nil
}

// writeLoad updates random rows at p.writes per second, half shared and half
// per-user, with a small worker pool. A write the pool cannot start on time
// is skipped and counted, so the rate stays honest.
func writeLoad(ctx context.Context, pool *pgxpool.Pool, p params, done, skipped *atomic.Int64) {
	if p.writes <= 0 {
		return
	}
	jobs := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range jobs {
				var err error
				if rand.IntN(2) == 0 {
					_, err = pool.Exec(ctx, `UPDATE cometload.shared_items SET n = n + 1 WHERE id = $1`, rand.IntN(p.groups*p.rows)+1)
				} else {
					_, err = pool.Exec(ctx, `
						UPDATE cometload.user_items SET n = n + 1 WHERE id = (
						  SELECT id FROM cometload.user_items WHERE owner = $1 AND bucket = $2 ORDER BY id LIMIT 1 OFFSET $3)`,
						fmt.Sprintf("u%d", rand.IntN(p.users)), rand.IntN(max(p.userSubs(), 1)), rand.IntN(p.rows))
				}
				if err == nil {
					done.Add(1)
				}
			}
		})
	}
	tick := time.NewTicker(time.Second / time.Duration(p.writes))
	defer tick.Stop()
	defer wg.Wait()
	defer close(jobs)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			select {
			case jobs <- struct{}{}:
			default:
				skipped.Add(1)
			}
		}
	}
}

// callLoad makes p.calls calls per second from random clients: 30% bump a
// shared row, 30% bump one of the caller's rows, 20% add a row (and drop the
// bucket's oldest), 20% ping, which writes nothing.
func callLoad(ctx context.Context, clients []*client, p params, skipped *atomic.Int64) {
	if p.calls <= 0 {
		return
	}
	tick := time.NewTicker(time.Second / time.Duration(p.calls))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c := clients[rand.IntN(len(clients))]
		if c.closed.Load() || c.inflight() >= 32 {
			skipped.Add(1)
			continue
		}
		var err error
		switch r := rand.IntN(10); {
		case r < 3:
			err = c.call("shared.bump", idArgs{ID: int64(rand.IntN(p.groups*p.rows) + 1)})
		case r < 6 && p.userSubs() > 0:
			err = c.call("user.bump", bucketArgs{B: rand.IntN(p.userSubs())})
		case r < 8 && p.userSubs() > 0:
			err = c.call("user.add", bucketArgs{B: rand.IntN(p.userSubs())})
		default:
			err = c.call("ping", struct{}{})
		}
		if err != nil {
			skipped.Add(1)
		}
	}
}

type slowFnStats struct {
	mu        sync.Mutex
	measuring bool
	latency   []float64
	failed    int
}

func (s *slowFnStats) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.measuring, s.latency, s.failed = true, nil, 0
}

func (s *slowFnStats) snapshot() *slowFnStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &slowFnStats{latency: append([]float64(nil), s.latency...), failed: s.failed}
}

// slowFnLoad calls slow.fn back to back on its own client, so there is
// always one in flight. Its results are kept apart from the other calls.
func slowFnLoad(ctx context.Context, c *client, s *slowFnStats) {
	for ctx.Err() == nil && !c.closed.Load() {
		c.stats.begin()
		start := time.Now()
		if err := c.call("slow.fn", struct{}{}); err != nil {
			return
		}
		for c.inflight() > 0 && ctx.Err() == nil && !c.closed.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		c.stats.mu.Lock()
		ok := c.stats.answered > 0 && len(c.stats.errs) == 0
		c.stats.mu.Unlock()
		s.mu.Lock()
		if s.measuring && ctx.Err() == nil {
			if ok {
				s.latency = append(s.latency, time.Since(start).Seconds())
			} else {
				s.failed++
			}
		}
		s.mu.Unlock()
	}
}

type server struct {
	cmd  *exec.Cmd
	addr string
}

// startServer runs this binary with -serve and waits for its address.
func startServer(ctx context.Context, p params) (*server, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, exe, "-serve", "-db", p.dsn,
		"-slow-observer", p.slowObserver.String(), "-slow-fn", p.slowFn.String())
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "addr ")
	if err != nil || !ok {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("server did not start (%q): %v", line, err)
	}
	return &server{cmd: cmd, addr: addr}, nil
}

func (s *server) stop() {
	s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.cmd.Process.Kill()
		<-done
	}
}

func (s *server) stats(reset bool) (*serverStats, error) {
	url := "http://" + s.addr + "/stats"
	if reset {
		url += "?reset"
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st serverStats
	return &st, json.NewDecoder(resp.Body).Decode(&st)
}
