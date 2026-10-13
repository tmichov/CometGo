package cometgo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Registering after Start would race the engine's unlocked map reads.
func TestRegisterAfterStartPanics(t *testing.T) {
	e := setup(t) // started
	const want = "cometgo: register publications and methods before Start"
	for name, register := range map[string]func(){
		"Publish": func() {
			Publish(e.app, "late", PubOptions{Tables: []string{e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
		},
		"Method": func() {
			Method(e.app, "late", func(*Call, struct{}) (struct{}, error) { return struct{}{}, nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != want {
					t.Fatalf("panic = %v, want %q", r, want)
				}
			}()
			register()
		})
	}
}

// Before Start, ServeHTTP refuses connections, so none can read the
// publication and method maps while they may still be registered.
func TestServeBeforeStartRefuses(t *testing.T) {
	e := setup(t) // for its database and table
	app := New(Config{DB: e.pool})
	Publish(app, "p", PubOptions{Tables: []string{e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dial := func() (*http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
		if err == nil {
			ws.CloseNow()
		}
		return resp, err
	}

	if resp, err := dial(); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before Start: err %v, resp %v; want 503", err, resp)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dial(); err != nil {
		t.Fatalf("after Start: %v", err)
	}
}
