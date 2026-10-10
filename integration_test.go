package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests need Postgres. Run `docker compose up -d` and set
// COMETGO_TEST_DATABASE_URL=postgres://comet:comet@localhost:54329/comet.

type testEnv struct {
	app   *App
	srv   *httptest.Server
	pool  *pgxpool.Pool
	table string
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	return setupWith(t, nil)
}

// setupWith is setup with Config overrides applied before New.
func setupWith(t *testing.T, tweak func(*Config)) *testEnv {
	t.Helper()
	dsn := os.Getenv("COMETGO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMETGO_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	table := "items_" + newID(4)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigserial PRIMARY KEY, owner text NOT NULL, name text NOT NULL, n int NOT NULL DEFAULT 0)`, table)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE "+table) })
	if err := InstallTriggers(ctx, pool, table); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		DB:              pool,
		MinTickInterval: 5 * time.Millisecond,
		Authenticate: func(ctx context.Context, token string) (*Identity, error) {
			switch token {
			case "alice", "bob":
				return &Identity{UserID: token}, nil
			case "alice-short":
				return &Identity{UserID: "alice", ExpiresAt: time.Now().Add(300 * time.Millisecond)}, nil
			}
			return nil, NewError(CodeUnauthenticated, "bad token")
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	app := New(cfg)
	Publish(app, "items.all", PubOptions{Tables: []string{table}, Scope: ScopeShared},
		func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT id, owner, name, n FROM " + table + " ORDER BY id"}, nil
		})
	Publish(app, "items.mine", PubOptions{Tables: []string{table}},
		func(c *SubCtx, _ struct{}) (Query, error) {
			if c.Identity == nil {
				return Query{}, ErrForbidden
			}
			return Query{SQL: "SELECT id, name FROM " + table + " WHERE owner = $1 ORDER BY id", Args: []any{c.Identity.UserID}}, nil
		})
	type addArgs struct {
		Owner, Name string
	}
	Method(app, "add", func(c *Call, a addArgs) (string, error) {
		if a.Name == "" {
			return "", ValidationError(map[string]string{"name": "required"})
		}
		var id string
		err := c.Tx.QueryRow(c.Context, "INSERT INTO "+table+" (owner, name) VALUES ($1, $2) RETURNING id::text", a.Owner, a.Name).Scan(&id)
		return id, err
	})
	Method(app, "bump", func(c *Call, a struct{ ID string }) (struct{}, error) {
		_, err := c.Tx.Exec(c.Context, "UPDATE "+table+" SET n = n + 1 WHERE id = $1::int8", a.ID)
		return struct{}{}, err
	})
	app.Start(ctx)
	// LISTEN comes up asynchronously; a fence notified before it would be
	// lost and its call would wait FenceTimeout. The listener signals
	// readiness with its first notification.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		app.engine.mu.Lock()
		up := app.engine.nextN > 0
		app.engine.mu.Unlock()
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not start")
		}
	}
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return &testEnv{app: app, srv: srv, pool: pool, table: table}
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func (e *testEnv) dial(t *testing.T, hello map[string]any) (*client, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	ws.SetReadLimit(1 << 20)
	c := &client{t: t, ws: ws}
	if hello == nil {
		hello = map[string]any{}
	}
	hello["t"], hello["v"] = "hello", 1
	c.send(hello)
	w := c.next()
	if w["t"] != "welcome" {
		t.Fatalf("expected welcome, got %v", w)
	}
	return c, w
}

func (c *client) send(m map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(m)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) next() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := c.ws.Read(ctx)
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			c.t.Fatal(err)
		}
		if m["t"] == "pong" {
			continue
		}
		return m
	}
}

func entries(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range m["subs"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestUpgradeRequiresSubprotocol(t *testing.T) {
	e := setup(t)
	resp, err := http.Get(e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status %d, want 426", resp.StatusCode)
	}
}

func TestSnapshotLiveDiffAndReadYourWrites(t *testing.T) {
	e := setup(t)
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'first')"); err != nil {
		t.Fatal(err)
	}

	a, _ := e.dial(t, nil)
	b, _ := e.dial(t, nil)
	for _, c := range []*client{a, b} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
		tx := c.next()
		en := entries(tx)
		if tx["t"] != "tx" || len(en) != 1 || en[0]["ready"] != true || en[0]["init"] == nil {
			t.Fatalf("bad snapshot %v", tx)
		}
		init := en[0]["init"].(map[string]any)
		if fmt.Sprint(init["cols"]) != "[[id i8] [owner text] [name text] [n i4]]" {
			t.Fatalf("cols %v", init["cols"])
		}
		ops := en[0]["ops"].([]any)
		if len(ops) != 1 || fmt.Sprint(ops[0]) != fmt.Sprintf("[i %s [%s alice first 0] 0]", firstKey(ops), firstKey(ops)) {
			t.Fatalf("ops %v", ops)
		}
	}

	// A's call: its tx must arrive before its res (§6.2).
	a.send(map[string]any{"t": "call", "id": 7, "m": "add", "args": map[string]any{"Owner": "bob", "Name": "second"}})
	tx := a.next()
	if tx["t"] != "tx" {
		t.Fatalf("expected tx before res, got %v", tx)
	}
	op := entries(tx)[0]["ops"].([]any)[0].([]any)
	if op[0] != "i" || op[3] != float64(1) {
		t.Fatalf("op %v", op)
	}
	res := a.next()
	if res["t"] != "res" || res["id"] != float64(7) || res["ok"] != true || res["val"] != op[1] {
		t.Fatalf("res %v (op key %v)", res, op[1])
	}
	if res["seq"].(float64) <= tx["seq"].(float64) {
		t.Fatalf("seq not increasing: tx %v res %v", tx["seq"], res["seq"])
	}

	// B, a different connection on the same shared observer, sees it too.
	if btx := b.next(); btx["t"] != "tx" || fmt.Sprint(entries(btx)[0]["ops"]) != fmt.Sprint(entries(tx)[0]["ops"]) {
		t.Fatalf("b got %v", btx)
	}

	// Update sends only the changed column.
	a.send(map[string]any{"t": "call", "id": 8, "m": "bump", "args": map[string]any{"ID": op[1]}})
	utx := a.next()
	if got := fmt.Sprint(entries(utx)[0]["ops"]); got != fmt.Sprintf("[[u %s [3 1]]]", op[1]) {
		t.Fatalf("update ops %s", got)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}

	// Validation errors come back structured, with no tx.
	a.send(map[string]any{"t": "call", "id": 9, "m": "add", "args": map[string]any{"Owner": "x", "Name": ""}})
	if r := a.next(); r["t"] != "res" || r["ok"] != false || r["err"].(map[string]any)["code"] != CodeValidation {
		t.Fatalf("res %v", r)
	}
}

func firstKey(ops []any) string { return ops[0].([]any)[1].(string) }

func TestUnknownPublicationEnds(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 3, "name": "nope"})
	en := entries(a.next())
	if en[0]["s"] != float64(3) || en[0]["end"].(map[string]any)["code"] != CodeNotFound {
		t.Fatalf("got %v", en)
	}
}

func TestIdentityChangeResetsSubsBeforeAuthRes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'a1'), ('bob', 'b1')"); err != nil {
		t.Fatal(err)
	}
	a, w := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	if fmt.Sprint(w["user"]) != "map[id:alice]" {
		t.Fatalf("welcome user %v", w["user"])
	}
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	if ops := entries(a.next())[0]["ops"].([]any); len(ops) != 1 || ops[0].([]any)[2].([]any)[1] != "a1" {
		t.Fatalf("alice ops %v", ops)
	}

	a.send(map[string]any{"t": "auth", "id": 2, "token": "bob"})
	tx := a.next()
	en := entries(tx)
	if tx["t"] != "tx" || en[0]["reset"] != true || en[0]["ready"] != true {
		t.Fatalf("expected reset tx, got %v", tx)
	}
	if ops := en[0]["ops"].([]any); len(ops) != 1 || ops[0].([]any)[2].([]any)[1] != "b1" {
		t.Fatalf("bob ops %v", ops)
	}
	if r := a.next(); r["t"] != "authRes" || r["ok"] != true || fmt.Sprint(r["user"]) != "map[id:bob]" {
		t.Fatalf("authRes %v", r)
	}

	// Logout: the user-scoped publication now refuses.
	a.send(map[string]any{"t": "auth", "id": 3, "token": nil})
	en = entries(a.next())
	if en[0]["end"].(map[string]any)["code"] != CodeForbidden {
		t.Fatalf("expected FORBIDDEN end, got %v", en)
	}
	if r := a.next(); r["t"] != "authRes" || r["user"] != nil {
		t.Fatalf("authRes %v", r)
	}
}

func TestBadTokenInHelloIsAnonymous(t *testing.T) {
	e := setup(t)
	a, w := e.dial(t, map[string]any{"auth": map[string]any{"token": "mallory"}})
	if w["user"] != nil {
		t.Fatalf("user %v", w["user"])
	}
	if r := a.next(); r["t"] != "authRes" || r["ok"] != false || r["err"].(map[string]any)["code"] != CodeUnauthenticated {
		t.Fatalf("authRes %v", r)
	}
}

func TestBulkChangeUsesTableDirty(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) SELECT 'x', 'n' || g FROM generate_series(1, 1500) g"); err != nil {
		t.Fatal(err)
	}
	total := 0
	for total < 1500 {
		tx := a.next()
		for _, en := range entries(tx) {
			if ops, ok := en["ops"].([]any); ok {
				total += len(ops)
			}
		}
	}
	if total != 1500 {
		t.Fatalf("got %d inserts", total)
	}
}
