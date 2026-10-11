package cometgo

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// setupTenants registers "tenant.items", a ScopeUser publication whose
// query depends on the tenant in ScopeKey, not only on UserID.
func setupTenants(t *testing.T) *testEnv {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "tenant.items", PubOptions{Tables: []string{env.table}, Events: true},
			func(c *SubCtx, _ struct{}) (Query, error) {
				if c.Identity == nil {
					return Query{}, ErrForbidden
				}
				return Query{SQL: "SELECT id, name FROM " + env.table + " WHERE owner = $1 ORDER BY id", Args: []any{c.Identity.ScopeKey}}, nil
			})
	})
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice@acme', 'a1'), ('alice@globex', 'g1')"); err != nil {
		t.Fatal(err)
	}
	return e
}

func names(en map[string]any) string {
	var out []any
	for _, op := range en["ops"].([]any) {
		out = append(out, op.([]any)[2].([]any)[1])
	}
	return fmt.Sprint(out)
}

// Same UserID, different ScopeKey: separate observers, separate rows.
func TestScopeKeySeparatesObservers(t *testing.T) {
	e := setupTenants(t)
	acme, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@acme"}})
	globex, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@globex"}})
	for c, want := range map[*client]string{acme: "[a1]", globex: "[g1]"} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "tenant.items"})
		if got := names(entries(c.next())[0]); got != want {
			t.Fatalf("snapshot %s, want %s", got, want)
		}
	}
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice@acme', 'a2')"); err != nil {
		t.Fatal(err)
	}
	if got := names(entries(acme.next())[0]); got != "[a2]" {
		t.Fatalf("acme ops %s", got)
	}
	globex.quiet(200 * time.Millisecond)
}

// Switching tenant under the same UserID is a different principal: every
// sub is rebound and reset to the new tenant's rows.
func TestScopeKeyChangeRebinds(t *testing.T) {
	e := setupTenants(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice@acme"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "tenant.items"})
	a.next()
	a.send(map[string]any{"t": "auth", "id": 1, "token": "alice@globex"})
	en := entries(a.next())[0]
	if en["reset"] != true || names(en) != "[g1]" {
		t.Fatalf("expected reset to globex rows, got %v", en)
	}
	if r := a.next(); r["t"] != "authRes" || r["ok"] != true {
		t.Fatalf("authRes %v", r)
	}
}

// A session cannot be resumed under another ScopeKey of the same user.
func TestScopeKeyResumeRefused(t *testing.T) {
	e := setupTenants(t)
	a, w := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice@acme"}}))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "tenant.items"})
	last := a.next()
	a.kill()
	time.Sleep(50 * time.Millisecond)
	resume := map[string]any{"session": w["session"], "seq": seqOf(last), "subs": []int{1}}
	_, w2 := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice@globex"}, "resume": resume}))
	if w2["resumed"] != false {
		t.Fatalf("resumed under another tenant: %v", w2)
	}
}

// Emit's scope is the ScopeKey: it reaches only that tenant's subs.
func TestScopeKeyEmitTargets(t *testing.T) {
	e := setupTenants(t)
	acme, _ := e.dial(t, evtHello("alice@acme"))
	globex, _ := e.dial(t, evtHello("alice@globex"))
	subscribe(t, acme, 1, "tenant.items")
	subscribe(t, globex, 1, "tenant.items")
	ctx := context.Background()
	for _, scope := range []string{"alice", "alice@globex"} {
		if err := e.app.Emit(ctx, "tenant.items", nil, scope, "for-"+scope, nil); err != nil {
			t.Fatal(err)
		}
	}
	expectEvt(t, globex.next(), 1, "for-alice@globex")
	acme.quiet(200 * time.Millisecond)
}
