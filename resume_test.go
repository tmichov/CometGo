package cometgo

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session resume (PROTOCOL.md §7.2). Needs Postgres, like integration_test.go.

func resumeHello(extra map[string]any) map[string]any {
	h := map[string]any{"caps": []string{capResume}}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// kill drops the client's socket without a close handshake, as a network
// failure would. The server is untouched.
func (c *client) kill() { c.ws.CloseNow() }

// collect reads until done reports true for the messages read so far.
func (c *client) collect(done func([]map[string]any) bool) []map[string]any {
	c.t.Helper()
	var got []map[string]any
	for !done(got) {
		got = append(got, c.next())
	}
	return got
}

// sync waits until the server has processed everything sent so far: a
// ping is answered after the messages before it.
func (c *client) sync() {
	c.t.Helper()
	c.send(map[string]any{"t": "ping", "n": "sync"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := c.ws.Read(ctx)
		if err != nil {
			c.t.Fatalf("sync: %v", err)
		}
		if strings.Contains(string(b), `"n":"sync"`) {
			return
		}
	}
}

// quiet fails if anything but pong arrives within d.
func (c *client) quiet(d time.Duration) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		_, b, err := c.ws.Read(ctx)
		if err != nil {
			return // deadline: nothing arrived
		}
		if !strings.Contains(string(b), `"t":"pong"`) {
			c.t.Fatalf("unexpected message %s", b)
		}
	}
}

func (e *testEnv) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+e.table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *testEnv) waitCount(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); e.count(t) != n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("table never reached %d rows", n)
		}
	}
}

func (e *testEnv) session(t *testing.T, id any) *conn {
	t.Helper()
	c := e.app.lookupSession(id.(string))
	if c == nil {
		t.Fatalf("session %v not registered", id)
	}
	return c
}

func seqOf(m map[string]any) uint64 { return uint64(m["seq"].(float64)) }

func TestWelcomeAdvertisesResume(t *testing.T) {
	e := setup(t)
	_, w := e.dial(t, nil)
	if fmt.Sprint(w["caps"]) != "[resume]" || w["resumed"] != false {
		t.Fatalf("welcome %v", w)
	}
	// A client without the cap gets no session kept for it.
	if e.app.lookupSession(w["session"].(string)) != nil {
		t.Fatal("session registered for a client without the resume cap")
	}
}

// Acceptance: the socket dies mid-stream; the resumed client gets the
// missed tx and res, with no reset and no second execution of the call.
func TestResumeReplaysMissedTxAndRes(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	if snap["t"] != "tx" || entries(snap)[0]["ready"] != true {
		t.Fatalf("snapshot %v", snap)
	}

	// The call commits, but its tx and res never reach the client.
	a.send(map[string]any{"t": "call", "id": 7, "m": "add", "args": map[string]any{"Owner": "a", "Name": "during"}})
	e.waitCount(t, 1)
	time.Sleep(50 * time.Millisecond)
	a.kill()
	// More data while the client is away.
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'gap')"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != true || w2["session"] != w["session"] {
		t.Fatalf("welcome %v", w2)
	}
	// As the client does after resumed:true: re-send the unanswered call and the sub.
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	b.send(map[string]any{"t": "call", "id": 7, "m": "add", "args": map[string]any{"Owner": "a", "Name": "during"}})

	var names []string
	res := 0
	last := seqOf(snap)
	b.collect(func(got []map[string]any) bool {
		if len(got) == 0 {
			return false
		}
		m := got[len(got)-1]
		if s := seqOf(m); s <= last {
			t.Fatalf("seq %d after %d: %v", s, last, m)
		} else {
			last = s
		}
		switch m["t"] {
		case "tx":
			for _, en := range entries(m) {
				if en["reset"] != nil || en["init"] != nil {
					t.Fatalf("resume sent a snapshot: %v", m)
				}
				for _, op := range en["ops"].([]any) {
					names = append(names, op.([]any)[2].([]any)[2].(string))
				}
			}
		case "res":
			if m["id"] != float64(7) || m["ok"] != true {
				t.Fatalf("res %v", m)
			}
			res++
		default:
			t.Fatalf("unexpected %v", m)
		}
		return res == 1 && len(names) == 2
	})
	if fmt.Sprint(names) != "[during gap]" {
		t.Fatalf("replayed inserts %v", names)
	}
	b.quiet(300 * time.Millisecond)
	if n := e.count(t); n != 2 {
		t.Fatalf("call ran again: %d rows", n)
	}
}

func TestResumeAckTrimsAndPrunesSubs(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.all"})
	got := a.collect(func(got []map[string]any) bool {
		n := 0
		for _, m := range got {
			n += len(entries(m))
		}
		return n == 2
	})
	last := seqOf(got[len(got)-1])
	c := e.session(t, w["session"])
	c.mu.Lock()
	buffered := len(c.replay)
	c.mu.Unlock()
	if buffered != len(got) {
		t.Fatalf("replay holds %d messages, want %d", buffered, len(got))
	}

	a.send(map[string]any{"t": "ack", "seq": last})
	a.send(map[string]any{"t": "ack", "seq": last + 100}) // never sent: ignored
	a.sync()
	c.mu.Lock()
	buffered, acked := len(c.replay), c.acked
	c.mu.Unlock()
	if buffered != 0 || acked != last {
		t.Fatalf("after ack: %d buffered, acked %d", buffered, acked)
	}

	// The client drops sub 2 but its unsub is lost: resume lists only sub 1.
	a.kill()
	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": last, "subs": []int{1}}}))
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'y')"); err != nil {
		t.Fatal(err)
	}
	en := entries(b.next())
	if len(en) != 1 || en[0]["s"] != float64(1) {
		t.Fatalf("pruned sub still bound: %v", en)
	}

	// Resuming from before the ack is impossible: the server no longer has those messages.
	b.kill()
	_, w3 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": last - 1, "subs": []int{1}}}))
	if w3["resumed"] != false || w3["session"] == w["session"] {
		t.Fatalf("welcome %v", w3)
	}
}

// The client saw its socket die before the server did: resume takes the
// session over and closes the old socket.
func TestResumeReplacesStillOpenSocket(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := a.ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("old socket was not closed")
			}
			break
		}
	}
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'y')"); err != nil {
		t.Fatal(err)
	}
	if tx := b.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
		t.Fatalf("resumed socket got %v", tx)
	}
}

func TestResumeOverflowWhileDetached(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.ResumeBuffer = 4 << 10 })
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	a.kill()
	time.Sleep(50 * time.Millisecond)
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) SELECT 'x', repeat('n', 100) FROM generate_series(1, 100)"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); e.app.lookupSession(w["session"].(string)) != nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("overflowing detached session was not ended")
		}
	}
	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != false {
		t.Fatalf("welcome %v", w2)
	}
	// The fallback (§7.1) still works on the new session.
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all", "have": true})
	if en := entries(b.next()); en[0]["reset"] != true || len(en[0]["ops"].([]any)) != 100 {
		t.Fatalf("resync %v", en)
	}
}

func TestResumeOverflowWhileAttachedRecoversAfterAck(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.ResumeBuffer = 4 << 10 })
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	a.send(map[string]any{"t": "call", "id": 9, "m": "nope", "noFence": true})
	if r := a.next(); r["t"] != "res" {
		t.Fatalf("res %v", r)
	}
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) SELECT 'x', repeat('n', 100) FROM generate_series(1, 100)"); err != nil {
		t.Fatal(err)
	}
	tx := a.next() // delivered: the socket is fine, only the buffer overflowed
	c := e.session(t, w["session"])
	c.mu.Lock()
	floor, ids := c.floor, len(c.ids)
	c.mu.Unlock()
	if floor != seqOf(tx) || ids != 0 {
		t.Fatalf("floor %d (want %d), %d ids kept", floor, seqOf(tx), ids)
	}
	a.send(map[string]any{"t": "ack", "seq": seqOf(tx)})
	a.sync()
	a.kill()
	_, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(tx), "subs": []int{1}}}))
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
}

func TestResumeAfterRestartFails(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(nil))
	a.kill()
	// A restarted server is a new App: new epoch, no sessions.
	restarted := setup(t)
	_, w2 := restarted.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": 0, "subs": []int{}}}))
	if w2["resumed"] != false || w2["epoch"] == w["epoch"] {
		t.Fatalf("welcome %v (old %v)", w2, w)
	}
}

func TestResumeAfterExpiryInGap(t *testing.T) {
	e := setup(t)
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'a1')"); err != nil {
		t.Fatal(err)
	}
	a, w := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice-short"}}))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	snap := a.next()
	a.kill()
	time.Sleep(500 * time.Millisecond) // the token expires while detached

	// Another principal cannot take the session over.
	_, wb := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "bob"},
		"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if wb["resumed"] != false {
		t.Fatalf("bob resumed alice's session: %v", wb)
	}

	// Fresh setup for the positive case: the session is now anonymous, and
	// an anonymous resume replays the expiry.
	e = setup(t)
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'a1')"); err != nil {
		t.Fatal(err)
	}
	a, w = e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice-short"}}))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	snap = a.next()
	a.kill()
	time.Sleep(500 * time.Millisecond)
	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != true || w2["user"] != nil {
		t.Fatalf("welcome %v", w2)
	}
	tx := b.next()
	if en := entries(tx); tx["t"] != "tx" || en[0]["end"].(map[string]any)["code"] != CodeForbidden {
		t.Fatalf("expected replayed FORBIDDEN end, got %v", tx)
	}
	if r := b.next(); r["t"] != "authRes" || r["ok"] != false || r["err"].(map[string]any)["code"] != CodeAuthExpired {
		t.Fatalf("expected replayed AUTH_EXPIRED, got %v", r)
	}
}

// TestClientResumeEndToEnd runs the TypeScript client against this server
// (client/test/resume.e2e.ts). It needs node on PATH.
func TestClientResumeEndToEnd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "client/test/resume.e2e.ts", "ws"+strings.TrimPrefix(e.srv.URL, "http")).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := e.count(t); n != 1 {
		t.Fatalf("call ran %d times", n)
	}
}

func TestResumeAfterGraceFails(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.ResumeGrace = 100 * time.Millisecond })
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	a.kill()
	for deadline := time.Now().Add(5 * time.Second); e.app.lookupSession(w["session"].(string)) != nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("session outlived its grace window")
		}
	}
	_, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != false {
		t.Fatalf("welcome %v", w2)
	}
}

// A sub the server ended during the gap: the client re-sends it before the
// replayed end reaches it. That re-send must not bind a sub the client will
// never track again.
func TestResumeDoesNotRebindSubEndedInGap(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxRows = 5 })
	a, w := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	a.kill()
	time.Sleep(50 * time.Millisecond)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) SELECT 'x', 'n' FROM generate_series(1, 10)"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // RESULT_TOO_LARGE ends sub 1
	if _, err := e.pool.Exec(ctx, "DELETE FROM "+e.table); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	b, w2 := e.dial(t, resumeHello(map[string]any{"resume": map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}}))
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all", "have": true})
	if en := entries(b.next()); en[0]["end"].(map[string]any)["code"] != CodeResultTooLarge {
		t.Fatalf("expected replayed end, got %v", en)
	}
	b.quiet(300 * time.Millisecond) // no snapshot for a re-bound sub 1
}

// A session ended under a live socket (here: a failed resume naming it)
// closes that socket, so its client resyncs instead of waiting forever.
func TestEndedSessionClosesItsSocket(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice"}}))
	_, wb := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "bob"},
		"resume": map[string]any{"session": w["session"], "seq": 0, "subs": []int{}}}))
	if wb["resumed"] != false {
		t.Fatalf("welcome %v", wb)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := a.ws.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("alice's socket stayed open: %v", err)
	}
}

// A deliberate close will not resume, so the session ends at once.
func TestNormalCloseEndsSession(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, resumeHello(nil))
	a.ws.Close(websocket.StatusNormalClosure, "")
	for deadline := time.Now().Add(5 * time.Second); e.app.lookupSession(w["session"].(string)) != nil; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("session kept after a normal close")
		}
	}
}

func TestResumeBufferClampedToOutboundQueue(t *testing.T) {
	a := New(Config{DB: &pgxpool.Pool{}, ResumeBuffer: 64 << 20})
	if a.cfg.ResumeBuffer != maxQueuedBytes/2 {
		t.Fatalf("ResumeBuffer %d", a.cfg.ResumeBuffer)
	}
}
