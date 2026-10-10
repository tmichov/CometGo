package cometgo

import (
	"context"
	"slices"
	"testing"
	"time"
)

func (e *testEnv) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func okRes(t *testing.T, r map[string]any, id int) any {
	t.Helper()
	if r["t"] != "res" || r["id"] != float64(id) || r["ok"] != true {
		t.Fatalf("expected ok res %d, got %v", id, r)
	}
	return r["val"]
}

func errCode(t *testing.T, r map[string]any) string {
	t.Helper()
	if r["t"] != "res" || r["ok"] != false {
		t.Fatalf("expected failed res, got %v", r)
	}
	return r["err"].(map[string]any)["code"].(string)
}

func TestWelcomeAdvertisesIKCap(t *testing.T) {
	e := setup(t)
	if _, w := e.dial(t, nil); !slices.Contains(w["caps"].([]any), any("ik")) {
		t.Fatalf("caps %v lack ik", w["caps"])
	}
}

func TestIdempotentRetryReturnsStoredResult(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	args := map[string]any{"Owner": "alice", "Name": "once"}

	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": args, "ik": ik})
	first := okRes(t, a.next(), 1)

	// The retry arrives on a new connection, as after a disconnect.
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": args, "ik": ik})
	if second := okRes(t, b.next(), 1); second != first {
		t.Fatalf("retry val %v, first %v", second, first)
	}

	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'once'"); n != 1 {
		t.Fatalf("%d rows inserted, want 1", n)
	}
	if n := e.countRows(t, "SELECT count(*) FROM comet_idempotency WHERE ik = $1", ik); n != 1 {
		t.Fatalf("%d idempotency rows, want 1", n)
	}
}

func TestIdempotentConcurrentDuplicatesRunOnce(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	args := map[string]any{"Owner": "alice", "Name": "dup"}
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})

	// Both are in flight together: slowAdd holds its transaction for 300ms.
	a.send(map[string]any{"t": "call", "id": 1, "m": "slowAdd", "args": args, "ik": ik})
	b.send(map[string]any{"t": "call", "id": 2, "m": "slowAdd", "args": args, "ik": ik})
	va := okRes(t, a.next(), 1)
	vb := okRes(t, b.next(), 2)

	if va != vb {
		t.Fatalf("vals differ: %v vs %v", va, vb)
	}
	if n := e.slowRuns.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'dup'"); n != 1 {
		t.Fatalf("%d rows inserted, want 1", n)
	}
}

func TestIdempotentDuplicateRunsWhenFirstFails(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})

	// a claims the key first and fails after 300ms; b waits on it, then runs.
	a.send(map[string]any{"t": "call", "id": 1, "m": "slowAdd", "args": map[string]any{"Owner": "alice", "Name": ""}, "ik": ik})
	time.Sleep(50 * time.Millisecond)
	b.send(map[string]any{"t": "call", "id": 2, "m": "slowAdd", "args": map[string]any{"Owner": "alice", "Name": "second"}, "ik": ik})
	if code := errCode(t, a.next()); code != CodeValidation {
		t.Fatalf("code %s, want VALIDATION", code)
	}
	okRes(t, b.next(), 2)
	if n := e.slowRuns.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2", n)
	}
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'second'"); n != 1 {
		t.Fatalf("%d rows inserted, want 1", n)
	}
}

func TestIdempotencyKeyIsPerUser(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	alice, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	bob, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	anon, _ := e.dial(t, nil)

	alice.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "x"}, "ik": ik})
	va := okRes(t, alice.next(), 1)
	bob.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "bob", "Name": "x"}, "ik": ik})
	vb := okRes(t, bob.next(), 1)
	anon.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "", "Name": "x"}, "ik": ik})
	vn := okRes(t, anon.next(), 1)

	if va == vb || va == vn || vb == vn {
		t.Fatalf("users shared a result: alice %v bob %v anon %v", va, vb, vn)
	}
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'x'"); n != 3 {
		t.Fatalf("%d rows inserted, want 3", n)
	}
}

func TestIdempotencyKeyRejectsOtherMethod(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "m"}, "ik": ik})
	okRes(t, a.next(), 1)
	a.send(map[string]any{"t": "call", "id": 2, "m": "slowAdd", "args": map[string]any{"Owner": "alice", "Name": "m"}, "ik": ik})
	if code := errCode(t, a.next()); code != CodeBadRequest {
		t.Fatalf("code %s, want BAD_REQUEST", code)
	}
	if n := e.slowRuns.Load(); n != 0 {
		t.Fatalf("handler ran %d times, want 0", n)
	}
}

func TestIdempotentFailureIsNotStored(t *testing.T) {
	e := setup(t)
	ik := "ik-" + newID(8)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})

	// A failed call rolls back its claim, so a corrected retry runs.
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": ""}, "ik": ik})
	if code := errCode(t, a.next()); code != CodeValidation {
		t.Fatalf("code %s, want VALIDATION", code)
	}
	if n := e.countRows(t, "SELECT count(*) FROM comet_idempotency WHERE ik = $1", ik); n != 0 {
		t.Fatalf("%d idempotency rows after failure, want 0", n)
	}
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "ok"}, "ik": ik})
	okRes(t, a.next(), 2)
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'ok'"); n != 1 {
		t.Fatalf("%d rows inserted, want 1", n)
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	long := make([]byte, maxIKLen+1)
	for i := range long {
		long[i] = 'k'
	}
	for i, ik := range []string{"", string(long)} {
		a.send(map[string]any{"t": "call", "id": i + 1, "m": "add", "args": map[string]any{"Owner": "", "Name": "v"}, "ik": ik})
		if code := errCode(t, a.next()); code != CodeBadRequest {
			t.Fatalf("ik %q: code %s, want BAD_REQUEST", ik, code)
		}
	}
	// "ik": null is the same as no key.
	a.send(map[string]any{"t": "call", "id": 3, "m": "add", "args": map[string]any{"Owner": "", "Name": "v"}, "ik": nil})
	okRes(t, a.next(), 3)
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'v'"); n != 1 {
		t.Fatalf("%d rows inserted, want 1", n)
	}
}

func TestIdempotencyCleanupDeletesExpired(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old, fresh := "ik-old-"+newID(8), "ik-fresh-"+newID(8)
	if _, err := e.pool.Exec(ctx, `INSERT INTO comet_idempotency (ik, user_id, method, result, created_at)
		VALUES ($1, 'u', 'add', '1', now() - interval '25 hours'), ($2, 'u', 'add', '1', now())`, old, fresh); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(ctx, "DELETE FROM comet_idempotency WHERE ik = $1", fresh) })
	if err := e.app.deleteExpiredIK(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.countRows(t, "SELECT count(*) FROM comet_idempotency WHERE ik IN ($1, $2)", old, fresh); n != 1 {
		t.Fatalf("%d rows left, want 1", n)
	}
	if n := e.countRows(t, "SELECT count(*) FROM comet_idempotency WHERE ik = $1", fresh); n != 1 {
		t.Fatal("fresh row was deleted")
	}
}
