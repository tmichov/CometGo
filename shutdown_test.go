package cometgo

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestShutdownDrainsCallsThenCloses4503(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	// slowAdd holds its transaction ~300 ms after the insert.
	a.send(map[string]any{"t": "call", "id": 1, "m": "slowAdd", "args": map[string]any{"Owner": "x", "Name": "inflight"}})
	for e.slowRuns.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- e.app.Shutdown(ctx)
	}()

	g := a.next()
	if g["t"] != "goaway" || g["reason"] != "restart" {
		t.Fatalf("expected goaway, got %v", g)
	}
	retry := g["retryInMs"].(float64)
	if retry < 0 || retry > maxGoawayRetryMs {
		t.Fatalf("retryInMs %v", retry)
	}
	// A call after goaway is refused, carrying the same delay.
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "x", "Name": "late"}})
	var gotRes1, gotRes2 bool
	for !gotRes1 || !gotRes2 {
		m := a.next()
		if m["t"] != "res" {
			continue
		}
		switch m["id"].(float64) {
		case 1:
			if m["ok"] != true || m["lag"] == true {
				t.Fatalf("in-flight call %v", m)
			}
			gotRes1 = true
		case 2:
			err := m["err"].(map[string]any)
			after, _ := err["retryAfterMs"].(float64) // omitted when 0
			if err["code"] != CodeUnavailable || after != retry {
				t.Fatalf("late call %v", m)
			}
			gotRes2 = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := a.ws.Read(ctx)
		if err != nil {
			if st := websocket.CloseStatus(err); st != closeGoingAway {
				t.Fatalf("closed with %v (%v)", st, err)
			}
			break
		}
		if strings.Contains(string(b), `"t":"res"`) {
			t.Fatalf("unexpected %s", b)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := e.rowsNamed(t, "inflight"); n != 1 {
		t.Fatalf("in-flight call committed %d rows", n)
	}
	if n := e.rowsNamed(t, "late"); n != 0 {
		t.Fatalf("refused call committed %d rows", n)
	}
	// New connections are refused.
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial after shutdown: %v %v", resp, err)
	}
}

func TestShutdownTimesOutOnStuckCall(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "sleepAdd", "args": map[string]any{"Name": "stuck", "SleepMs": 2000}})
	for deadline := time.Now().Add(5 * time.Second); inflight(e) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("call never started")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.app.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Shutdown took %v past its deadline", d)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	for {
		if _, _, err := a.ws.Read(rctx); err != nil {
			if st := websocket.CloseStatus(err); st != closeGoingAway {
				t.Fatalf("closed with %v (%v)", st, err)
			}
			return
		}
	}
}

func inflight(e *testEnv) int32 {
	var n int32
	for _, c := range e.app.liveConns() {
		n += c.inflight.Load()
	}
	return n
}

// TestClientShutdownEndToEnd runs the TypeScript client through a shutdown
// (client/test/shutdown.e2e.ts): its in-flight call resolves, the socket
// closes with 4503, and it reconnects to the replacement server after
// about retryInMs. It needs node on PATH.
func TestClientShutdownEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	old, next := setup(t), setup(t)
	var cur atomic.Pointer[App]
	cur.Store(old.app)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cur.Load().ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "client/test/shutdown.e2e.ts", "ws"+strings.TrimPrefix(srv.URL, "http"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	shut := make(chan error, 1)
	var out []string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		out = append(out, line)
		if line == "inflight" {
			// The replacement is up before the old server goes away, as
			// in a rolling restart.
			cur.Store(next.app)
			go func() {
				sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer scancel()
				shut <- old.app.Shutdown(sctx)
			}()
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, strings.Join(out, "\n"), stderr.String())
	}
	if len(out) == 0 || out[len(out)-1] != "ok" {
		t.Fatalf("output:\n%s\n%s", strings.Join(out, "\n"), stderr.String())
	}
	if err := <-shut; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	t.Log(strings.Join(out, "\n"))
}
