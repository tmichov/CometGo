package cometgo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

var evtCaps = map[string]any{"caps": []string{"evt"}}

func evtHello(token string) map[string]any {
	h := map[string]any{"caps": []string{"evt"}}
	if token != "" {
		h["auth"] = map[string]any{"token": token}
	}
	return h
}

func subscribe(t *testing.T, c *client, s int, name string) {
	t.Helper()
	c.send(map[string]any{"t": "sub", "s": s, "name": name})
	if m := c.next(); m["t"] != "tx" || entries(m)[0]["ready"] != true {
		t.Fatalf("expected snapshot, got %v", m)
	}
}

func expectEvt(t *testing.T, m map[string]any, s int, topic string) {
	t.Helper()
	if m["t"] != "evt" || m["s"] != float64(s) || m["topic"] != topic {
		t.Fatalf("expected evt %s on sub %d, got %v", topic, s, m)
	}
}

func TestWelcomeAdvertisesEvtCap(t *testing.T) {
	e := setup(t)
	if _, w := e.dial(t, nil); !slices.Contains(w["caps"].([]any), any("evt")) {
		t.Fatalf("caps %v lack evt", w["caps"])
	}
}

func TestEmitAfterCommitArrivesBetweenTxAndRes(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, evtCaps)
	subscribe(t, a, 1, "items.all")

	a.send(map[string]any{"t": "call", "id": 5, "m": "addAndEmit", "args": map[string]any{"Owner": "x", "Name": "n"}})
	tx := a.next()
	if tx["t"] != "tx" {
		t.Fatalf("expected tx first, got %v", tx)
	}
	key := entries(tx)[0]["ops"].([]any)[0].([]any)[1]
	evt := a.next()
	expectEvt(t, evt, 1, "added")
	if id := evt["data"].(map[string]any)["id"]; id != key {
		t.Fatalf("evt data id %v, row key %v", id, key)
	}
	res := a.next()
	if res["t"] != "res" || res["ok"] != true {
		t.Fatalf("expected res last, got %v", res)
	}
	if !(tx["seq"].(float64) < evt["seq"].(float64) && evt["seq"].(float64) < res["seq"].(float64)) {
		t.Fatalf("seq order tx %v evt %v res %v", tx["seq"], evt["seq"], res["seq"])
	}
}

func TestEmitAfterCommitRollsBackWithMethod(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, evtCaps)
	subscribe(t, a, 1, "items.all")
	a.send(map[string]any{"t": "call", "id": 1, "m": "addAndEmit", "args": map[string]any{"Owner": "x", "Name": ""}})
	if r := a.next(); r["t"] != "res" || r["ok"] != false {
		t.Fatalf("expected failed res, got %v", r)
	}
	if err := e.app.Emit(context.Background(), "items.all", nil, "", "marker", 1); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, a.next(), 1, "marker") // no "added" before it
}

func TestEvtOnlyForClientsWithCap(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	a.send(map[string]any{"t": "call", "id": 1, "m": "addAndEmit", "args": map[string]any{"Owner": "x", "Name": "n"}})
	if m := a.next(); m["t"] != "tx" {
		t.Fatalf("expected tx, got %v", m)
	}
	if m := a.next(); m["t"] != "res" {
		t.Fatalf("expected res with no evt before it, got %v", m)
	}
}

func TestEmitTargetsMatchingObserver(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice, _ := e.dial(t, evtHello("alice"))
	bob, _ := e.dial(t, evtHello("bob"))
	subscribe(t, alice, 1, "items.mine")
	subscribe(t, bob, 2, "items.mine")
	subscribe(t, alice, 3, "items.all")

	// Other args and other users' scopes reach nobody here.
	if err := e.app.Emit(ctx, "items.mine", map[string]int{"x": 1}, "alice", "wrong-args", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Emit(ctx, "items.mine", nil, "carol", "wrong-user", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Emit(ctx, "items.mine", nil, "alice", "for-alice", map[string]any{"pct": 50}); err != nil {
		t.Fatal(err)
	}
	m := alice.next()
	expectEvt(t, m, 1, "for-alice")
	if fmt.Sprint(m["data"]) != "map[pct:50]" {
		t.Fatalf("data %v", m["data"])
	}
	// Shared scope ignores the scope argument.
	if err := e.app.Emit(ctx, "items.all", nil, "anyone", "shared", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, alice.next(), 3, "shared")
	if err := e.app.Emit(ctx, "items.mine", nil, "bob", "for-bob", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, bob.next(), 2, "for-bob") // not for-alice first
}

func TestEventsAreNotReplayedAfterReconnect(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, _ := e.dial(t, evtCaps)
	// watcher stays connected; seeing an event on it means the engine has
	// processed that event.
	watcher, _ := e.dial(t, evtCaps)
	subscribe(t, a, 1, "items.all")
	subscribe(t, watcher, 9, "items.all")
	if err := e.app.Emit(ctx, "items.all", nil, "", "first", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, a.next(), 1, "first")
	expectEvt(t, watcher.next(), 9, "first")
	a.ws.CloseNow()

	// Emitted while the client is away: it must not get it on return.
	if err := e.app.Emit(ctx, "items.all", nil, "", "missed", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, watcher.next(), 9, "missed")
	b, _ := e.dial(t, evtCaps)
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all", "have": true})
	if m := b.next(); m["t"] != "tx" || entries(m)[0]["reset"] != true {
		t.Fatalf("expected reset, got %v", m)
	}
	if err := e.app.Emit(ctx, "items.all", nil, "", "after", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, b.next(), 1, "after") // neither first nor missed
}

func TestEmitErrors(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	big := strings.Repeat("x", maxEvtPayload)
	if err := e.app.Emit(ctx, "items.all", nil, "", "big", big); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("err %v, want ErrEventTooLarge", err)
	}
	if err := e.app.Emit(ctx, "nope", nil, "", "t", nil); err == nil {
		t.Fatal("unknown publication accepted")
	}
	Publish(e.app, "items.quiet", PubOptions{Tables: []string{e.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
		return Query{SQL: "SELECT id FROM " + e.table}, nil
	})
	if err := e.app.Emit(ctx, "items.quiet", nil, "", "t", nil); err == nil || !strings.Contains(err.Error(), "Events") {
		t.Fatalf("err %v, want Events not enabled", err)
	}
}

func TestResumeReplaysMissedEventOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	caps := map[string]any{"caps": []string{capResume, capEvt}}
	a, w := e.dial(t, caps)
	watcher, _ := e.dial(t, evtCaps)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	snap := a.next()
	subscribe(t, watcher, 9, "items.all")
	a.kill()

	if err := e.app.Emit(ctx, "items.all", nil, "", "gap", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, watcher.next(), 9, "gap") // processed while a was away

	resume := map[string]any{"session": w["session"], "seq": seqOf(snap), "subs": []int{1}}
	b, w2 := e.dial(t, map[string]any{"caps": []string{capResume, capEvt}, "resume": resume})
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
	expectEvt(t, b.next(), 1, "gap")
	if err := e.app.Emit(ctx, "items.all", nil, "", "after", nil); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, b.next(), 1, "after")
}
