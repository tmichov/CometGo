package cometgo

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pauseProxy forwards TCP to target. While paused it stops forwarding
// without closing anything, like a network black hole; bytes read
// meanwhile are delivered on resume, so no stream is corrupted.
type pauseProxy struct {
	ln     net.Listener
	mu     sync.Mutex
	cond   *sync.Cond
	paused bool
}

func newPauseProxy(t *testing.T, target string) *pauseProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pauseProxy{ln: ln}
	p.cond = sync.NewCond(&p.mu)
	var conns []net.Conn
	var connsMu sync.Mutex
	t.Cleanup(func() {
		p.resume()
		ln.Close()
		connsMu.Lock()
		defer connsMu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			connsMu.Lock()
			conns = append(conns, c, d)
			connsMu.Unlock()
			go p.pipe(d, c)
			go p.pipe(c, d)
		}
	}()
	return p
}

func (p *pauseProxy) pipe(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			p.mu.Lock()
			for p.paused {
				p.cond.Wait()
			}
			p.mu.Unlock()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *pauseProxy) pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
}

func (p *pauseProxy) resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
	p.cond.Broadcast()
}

// setupProxied starts an app whose pool, and so its listener, reaches the
// database through a pauseProxy, with a short listener idle timeout.
func setupProxied(t *testing.T, tweak func(*Config)) (*testEnv, *pauseProxy) {
	var proxy *pauseProxy
	e := setupFull(t, func(c *Config) {
		cc := c.DB.Config()
		proxy = newPauseProxy(t, net.JoinHostPort(cc.ConnConfig.Host, strconv.Itoa(int(cc.ConnConfig.Port))))
		host, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
		n, _ := strconv.Atoi(port)
		cc.ConnConfig.Host, cc.ConnConfig.Port = host, uint16(n)
		for _, f := range cc.ConnConfig.Fallbacks { // e.g. sslmode=prefer's plain retry
			f.Host, f.Port = host, uint16(n)
		}
		pool, err := pgxpool.NewWithConfig(context.Background(), cc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
		if tweak != nil {
			tweak(c)
		}
	}, func(_ *testEnv, app *App) {
		app.engine.listenIdle, app.engine.listenProbe = 300*time.Millisecond, 200*time.Millisecond
	})
	// Before the app's own cleanup (LIFO), so a test that fails while paused
	// does not hang its shutdown.
	t.Cleanup(proxy.resume)
	return e, proxy
}

// A listener whose network path goes silent, with no TCP reset, notices
// within listenIdle + listenProbe and reconnects once the path is back.
func TestListenerDetectsSilentlyDeadConnection(t *testing.T) {
	log := &metricLog{}
	e, proxy := setupProxied(t, func(c *Config) { c.Metrics = log.hook })
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	e.waitListening(t)
	before := e.listened()

	proxy.pause()
	log.wait(t, MetricListenReconnect, nil) // the probe failed; the reconnect waits for the path
	proxy.resume()
	for deadline := time.Now().Add(5 * time.Second); e.listened() == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("listener did not reconnect")
		}
	}

	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'after')"); err != nil {
		t.Fatal(err)
	}
	for {
		if m := a.next(); m["t"] == "tx" && strings.Contains(fmt.Sprint(m), "after") {
			return
		}
	}
}

// While notifications keep arriving, the listener sends no probe.
func TestListenerProbesOnlyWhenIdle(t *testing.T) {
	appName := "cometgo_probe_" + newID(4)
	e, _ := setupProxied(t, withAppName(t, appName))
	e.waitListening(t)
	ctx := context.Background()
	var pid int32
	if err := e.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	lastQuery := func() string {
		var q string
		if err := e.pool.QueryRow(ctx, "SELECT query FROM pg_stat_activity WHERE pid = $1", pid).Scan(&q); err != nil {
			t.Fatal(err)
		}
		return q
	}

	for range 12 { // 1.2 s of traffic, each gap well under listenIdle
		if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'y')"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if q := lastQuery(); !strings.HasPrefix(q, "LISTEN ") {
		t.Fatalf("listener ran %q while notifications were flowing", q)
	}
	for deadline := time.Now().Add(3 * time.Second); lastQuery() != "SELECT 1"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("idle listener never probed")
		}
	}
}
