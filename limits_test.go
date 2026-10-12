package cometgo

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Over MaxObservers a sub that needs a new observer ends with OVERLOADED;
// one joining an existing observer is unaffected, and freeing one lets a new
// one in.
func TestMaxObservers(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxObservers = 1 })
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, nil)
	for _, c := range []*client{a, b} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
		if en := entries(c.next()); en[0]["ready"] != true {
			t.Fatalf("items.all %v", en)
		}
	}
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.mine"})
	en := entries(a.next())
	if end, _ := en[0]["end"].(map[string]any); en[0]["s"] != float64(2) || end["code"] != CodeOverloaded {
		t.Fatalf("expected OVERLOADED, got %v", en)
	}

	// Both unsubs free the only observer; b's may land after a's next sub.
	b.send(map[string]any{"t": "unsub", "s": 1})
	a.send(map[string]any{"t": "unsub", "s": 1})
	for s := 3; ; s++ {
		a.send(map[string]any{"t": "sub", "s": s, "name": "items.mine"})
		en := entries(a.next())
		if en[0]["ready"] == true {
			break
		}
		if end, _ := en[0]["end"].(map[string]any); end["code"] != CodeOverloaded || s > 50 {
			t.Fatalf("after unsub: %v", en)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// upgrade dials and returns the HTTP status of a refused upgrade, or 101.
func (e *testEnv) upgrade(t *testing.T, ip string) (int, *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http"), &websocket.DialOptions{
		Subprotocols: []string{Subprotocol},
		HTTPHeader:   http.Header{"X-Test-Ip": {ip}},
	})
	if err != nil {
		if resp == nil {
			t.Fatal(err)
		}
		return resp.StatusCode, nil
	}
	t.Cleanup(func() { ws.CloseNow() })
	return http.StatusSwitchingProtocols, ws
}

// waitUpgrade retries until an upgrade succeeds: a closed connection is
// released once the server sees the close.
func (e *testEnv) waitUpgrade(t *testing.T, ip string) {
	t.Helper()
	for range 100 {
		if st, _ := e.upgrade(t, ip); st == http.StatusSwitchingProtocols {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection slot never released")
}

func TestMaxConns(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxConns = 2 })
	_, first := e.upgrade(t, "")
	if st, _ := e.upgrade(t, ""); st != http.StatusSwitchingProtocols {
		t.Fatalf("second: %d", st)
	}
	if st, _ := e.upgrade(t, ""); st != http.StatusServiceUnavailable {
		t.Fatalf("over MaxConns: %d, want 503", st)
	}
	first.Close(websocket.StatusNormalClosure, "")
	e.waitUpgrade(t, "")
}

func TestMaxConnsPerIP(t *testing.T) {
	e := setupWith(t, func(c *Config) {
		c.MaxConnsPerIP = 1
		c.ClientIP = func(r *http.Request) string { return r.Header.Get("X-Test-Ip") }
	})
	_, first := e.upgrade(t, "1.1.1.1")
	if st, _ := e.upgrade(t, "2.2.2.2"); st != http.StatusSwitchingProtocols {
		t.Fatalf("other IP: %d", st)
	}
	if st, _ := e.upgrade(t, "1.1.1.1"); st != http.StatusServiceUnavailable {
		t.Fatalf("over MaxConnsPerIP: %d, want 503", st)
	}
	first.Close(websocket.StatusNormalClosure, "")
	e.waitUpgrade(t, "1.1.1.1")
}

// Without ClientIP, connections count against the host of RemoteAddr.
func TestMaxConnsPerIPDefaultsToRemoteAddr(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxConnsPerIP = 1 })
	if st, _ := e.upgrade(t, "1.1.1.1"); st != http.StatusSwitchingProtocols {
		t.Fatalf("first: %d", st)
	}
	if st, _ := e.upgrade(t, "2.2.2.2"); st != http.StatusServiceUnavailable {
		t.Fatalf("second from 127.0.0.1: %d, want 503", st)
	}
}

// At the cap, an fn's own error is reported, not OVERLOADED.
func TestMaxObserversReportsFnErrorFirst(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxObservers = 1 })
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.mine"}) // anonymous: FORBIDDEN
	if end, _ := entries(a.next())[0]["end"].(map[string]any); end["code"] != CodeForbidden {
		t.Fatalf("got %v, want FORBIDDEN", end)
	}
}

// An identity change frees the old identity's observers before binding the
// new ones, so swapping one user-scoped observer for another fits the cap.
func TestMaxObserversRebind(t *testing.T) {
	e := setupWith(t, func(c *Config) { c.MaxObservers = 2 })
	other, _ := e.dial(t, nil)
	other.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	other.next()
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	for s := 1; s <= 2; s++ { // two subs on one alice observer
		a.send(map[string]any{"t": "sub", "s": s, "name": "items.mine"})
		if en := entries(a.next()); en[0]["ready"] != true {
			t.Fatalf("alice sub %d: %v", s, en)
		}
	}
	a.send(map[string]any{"t": "auth", "id": 1, "token": "bob"})
	for {
		m := a.next()
		if m["t"] == "authRes" {
			break
		}
		for _, en := range entries(m) {
			if en["end"] != nil {
				t.Fatalf("sub %v ended on rebind: %v", en["s"], en["end"])
			}
		}
	}
}
