package cometgo

import "testing"

// fn runs for every subscriber of a ScopeShared publication, so an access
// check in it denies a later subscriber without disturbing the first.
func TestSharedPublicationChecksEverySubscriber(t *testing.T) {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "team", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, a struct{ Team string }) (Query, error) {
				if c.Identity == nil || c.Identity.UserID != "alice" {
					return Query{}, ErrForbidden // only alice is on the team
				}
				return Query{SQL: "SELECT id, name FROM " + env.table + " WHERE owner = $1 ORDER BY id", Args: []any{a.Team}}, nil
			})
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	sub := map[string]any{"t": "sub", "s": 1, "name": "team", "args": map[string]any{"Team": "red"}}
	a.send(sub)
	if en := entries(a.next()); en[0]["ready"] != true {
		t.Fatalf("alice %v", en)
	}
	b.send(sub)
	if en := entries(b.next()); en[0]["s"] != float64(1) || en[0]["end"] == nil || en[0]["end"].(map[string]any)["code"] != CodeForbidden {
		t.Fatalf("bob joined the shared observer: %v", en)
	}

	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "red", "Name": "r1"}})
	if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) || entries(tx)[0]["ops"] == nil {
		t.Fatalf("alice stopped streaming: %v", tx)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
}

// On a user change the rebind runs fn too: a sub whose observer another
// connection keeps alive is still re-checked for the new identity.
func TestSharedPublicationChecksOnRebind(t *testing.T) {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "team", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				if c.Identity == nil || c.Identity.UserID != "alice" {
					return Query{}, ErrForbidden
				}
				return Query{SQL: "SELECT id, name FROM " + env.table + " ORDER BY id"}, nil
			})
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	for _, c := range []*client{a, b} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "team"})
		if en := entries(c.next()); en[0]["ready"] != true {
			t.Fatalf("alice %v", en)
		}
	}
	b.send(map[string]any{"t": "auth", "id": 1, "token": "bob"})
	if en := entries(b.next()); en[0]["end"] == nil || en[0]["end"].(map[string]any)["code"] != CodeForbidden {
		t.Fatalf("bob kept the shared sub: %v", en)
	}
	if r := b.next(); r["t"] != "authRes" || r["ok"] != true {
		t.Fatalf("authRes %v", r)
	}
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
		t.Fatalf("alice stopped streaming: %v", tx)
	}
}
