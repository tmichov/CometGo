package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// smallBufListener shrinks the send buffer of accepted connections, so a
// client that stops reading backs up the server's own queue quickly instead
// of filling megabytes of kernel buffers first.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(16 << 10)
	}
	return c, err
}

// dialSlow is dial over small kernel buffers on both ends.
func (e *testEnv) dialSlow(t *testing.T, hello map[string]any) (*client, map[string]any) {
	t.Helper()
	srv := httptest.NewUnstartedServer(e.app)
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	dialer := &net.Dialer{}
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetReadBuffer(16 << 10)
			}
			return c, err
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{
		Subprotocols: []string{Subprotocol}, HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	ws.SetReadLimit(1 << 20)
	c := &client{t: t, ws: ws}
	hello["t"], hello["v"] = "hello", 1
	c.send(hello)
	w := c.next()
	if w["t"] != "welcome" {
		t.Fatalf("expected welcome, got %v", w)
	}
	return c, w
}

// mirror applies sub 1's tx entries the way a client store would.
type mirror struct {
	rows  map[string][]any // key → cells (id, owner, name, n)
	reset bool             // a reset entry was applied
}

func (m *mirror) apply(t *testing.T, msg map[string]any) {
	t.Helper()
	if msg["t"] != "tx" {
		return
	}
	for _, en := range entries(msg) {
		if en["s"].(float64) != 1 {
			continue
		}
		if en["end"] != nil {
			t.Fatalf("sub ended: %v", en["end"])
		}
		if en["init"] != nil {
			m.rows = map[string][]any{}
			if en["reset"] == true {
				m.reset = true
			}
		}
		ops, _ := en["ops"].([]any)
		for _, o := range ops {
			o := o.([]any)
			key := o[1].(string)
			switch o[0] {
			case "i":
				m.rows[key] = o[2].([]any)
			case "u":
				patch := o[2].([]any)
				for i := 0; i < len(patch); i += 2 {
					m.rows[key][int(patch[i].(float64))] = patch[i+1]
				}
			case "d":
				delete(m.rows, key)
			}
		}
	}
}

// matchesDB reports whether the mirror holds exactly the table's rows.
func (m *mirror) matchesDB(t *testing.T, e *testEnv) error {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), "SELECT id::text, name, n FROM "+e.table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for _, cells := range m.rows {
		got[fmt.Sprint(cells[0])] = fmt.Sprintf("%v/%v", cells[2], cells[3])
	}
	n := 0
	for rows.Next() {
		var id, name string
		var v int
		if err := rows.Scan(&id, &name, &v); err != nil {
			t.Fatal(err)
		}
		n++
		if want := fmt.Sprintf("%v/%v", name, v); got[id] != want {
			return fmt.Errorf("row %s: have %.20q, want %.20q", id, got[id], want)
		}
	}
	if n != len(got) {
		return fmt.Errorf("have %d rows, want %d", len(got), n)
	}
	return nil
}

// stalledClient subscribes to items.all over a 50-row, ~50 KiB result and
// returns the session and the mirror of its snapshot. The caller then stops
// reading.
func stalledClient(t *testing.T, e *testEnv) (*client, *conn, *mirror) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) SELECT 'x', repeat('a', 1000) FROM generate_series(1, 50)"); err != nil {
		t.Fatal(err)
	}
	a, w := e.dialSlow(t, resumeHello(nil)) // resume registers the session, so the test can find it
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	m := &mirror{}
	m.apply(t, a.next())
	if len(m.rows) != 50 {
		t.Fatalf("snapshot has %d rows", len(m.rows))
	}
	return a, e.session(t, w["session"]), m
}

// rewrite changes every row's 1000-byte name, a ~50 KiB diff per tick.
func (e *testEnv) rewrite(t *testing.T, i int) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), "UPDATE "+e.table+" SET name = repeat(chr(98 + $1::int % 25), 1000), n = n + 1", i); err != nil {
		t.Fatal(err)
	}
}

func TestSlowClientCoalescesToReset(t *testing.T) {
	e := setup(t)
	a, c, m := stalledClient(t, e)

	// Sample the server's queue for this connection while the client is
	// not reading.
	var peak atomic.Int64
	var coalesced atomic.Bool
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if q := int64(c.queuedBytes(0)); q > peak.Load() {
				peak.Store(q)
			}
			if c.coalescing.Load() {
				coalesced.Store(true)
			}
		}
	}()
	// 200 diffs of ~50 KiB: ~10 MiB, over the hard limit had they all been
	// queued. Kept under the 10 s write deadline.
	for i := range 200 {
		e.rewrite(t, i)
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	<-sampled
	t.Logf("peak queue %d bytes", peak.Load())
	if !coalesced.Load() {
		t.Fatal("connection never coalesced")
	}
	if p := peak.Load(); p >= maxQueuedBytes || p > 1<<20 {
		t.Fatalf("queue peaked at %d bytes, want bounded by result size", p)
	}

	// Reading again: the queued diffs, then one reset, converging on the
	// table. A 4429 close would fail the read.
	for !m.reset {
		m.apply(t, a.next())
	}
	a.sync()
	if err := m.matchesDB(t, e); err != nil {
		t.Fatal(err)
	}
	if c.coalescing.Load() {
		t.Fatal("still coalescing after drain")
	}
	// Live diffs resume after the reset.
	e.rewrite(t, 7)
	msg := a.next()
	if en := entries(msg); en[0]["reset"] != nil || en[0]["init"] != nil {
		t.Fatalf("expected a plain diff, got %v", en)
	}
	m.apply(t, msg)
	if err := m.matchesDB(t, e); err != nil {
		t.Fatal(err)
	}
}

func TestCoalescingResFollowsResetWithItsWrites(t *testing.T) {
	e := setup(t)
	a, c, m := stalledClient(t, e)
	for i := 0; !c.coalescing.Load(); i++ {
		if i == 200 {
			t.Fatal("connection never coalesced")
		}
		e.rewrite(t, i)
		time.Sleep(10 * time.Millisecond)
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), "SELECT min(id)::text FROM "+e.table).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a.send(map[string]any{"t": "call", "id": 1, "m": "bump", "args": map[string]any{"ID": id}})
	// The bump's fence releases this connection's held resets in the same
	// tick, so the res waits on nothing the client is not reading.
	time.Sleep(100 * time.Millisecond)

	for {
		msg := a.next()
		if msg["t"] == "res" {
			if msg["ok"] != true || msg["lag"] == true {
				t.Fatalf("res %v", msg)
			}
			break
		}
		m.apply(t, msg)
	}
	if !m.reset {
		t.Fatal("res arrived before any reset")
	}
	// Everything before the res reflects the bump: the client sees its
	// write, and the rows it had not been sent yet.
	if err := m.matchesDB(t, e); err != nil {
		t.Fatal(err)
	}
}

// The protocol never sends a reset for a sub whose init the client has not
// seen: a sub made while coalescing gets its first snapshot, without reset.
func TestCoalescingHoldsNewSubAsInit(t *testing.T) {
	e := setup(t)
	a, c, m := stalledClient(t, e)
	for i := 0; !c.coalescing.Load(); i++ {
		if i == 200 {
			t.Fatal("connection never coalesced")
		}
		e.rewrite(t, i)
		time.Sleep(10 * time.Millisecond)
	}
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.all"})
	time.Sleep(100 * time.Millisecond)
	var sub2 []map[string]any
	for !m.reset || sub2 == nil {
		msg := a.next()
		m.apply(t, msg)
		if msg["t"] == "tx" {
			for _, en := range entries(msg) {
				if en["s"].(float64) == 2 {
					sub2 = append(sub2, en)
				}
			}
		}
	}
	if sub2[0]["init"] == nil || sub2[0]["reset"] != nil {
		b, _ := json.Marshal(sub2[0])
		t.Fatalf("sub 2 first entry %s", b)
	}
}
