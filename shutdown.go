package cometgo

import (
	"context"
	"math/rand/v2"
	"time"
)

// maxGoawayRetryMs bounds the random reconnect delay sent in goaway, which
// spreads the clients' reconnects out (§7.3).
const maxGoawayRetryMs = 5000

// Shutdown stops the server gracefully (PROTOCOL.md §7.3):
//
//  1. Every session is sent goaway with a random retryInMs of 0–5 s, and
//     new WebSocket upgrades are refused with 503.
//  2. New calls are refused with UNAVAILABLE; calls already running finish
//     and their results are sent.
//  3. Once no call is in flight (or ctx is done), every socket is closed
//     with 4503 after what is queued for it has been written.
//  4. The engine, the listener and the idempotency cleanup stop.
//
// It returns ctx.Err() if ctx ended before the calls or the closes
// finished; the remaining sockets are then closed at once. A concurrent or
// later Shutdown waits for the first (or for its own ctx) and returns its
// result. Call it before http.Server.Shutdown, which does not wait for
// hijacked connections.
func (a *App) Shutdown(ctx context.Context) error {
	a.shutOnce.Do(func() {
		go func() {
			a.shutErr = a.shutdown(ctx)
			close(a.shutDone)
		}()
	})
	select {
	case <-a.shutDone:
		return a.shutErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) shutdown(ctx context.Context) error {
	a.shutting.Store(true)
	for _, c := range a.liveConns() {
		c.goaway()
	}

	err := a.drainCalls(ctx)

	var socks []*socket
	for _, c := range a.liveConns() {
		c.mu.Lock()
		if s := c.sock; s != nil {
			socks = append(socks, s)
		}
		c.mu.Unlock()
	}
	for _, s := range socks {
		s.closeAfterDrain(closeGoingAway, CodeUnavailable, "server shutting down")
	}
	for _, s := range socks {
		select {
		case <-s.ctx.Done():
		case <-ctx.Done():
			s.fail(closeGoingAway, CodeUnavailable, "server shutting down")
			err = ctx.Err()
		}
	}
	// The server will not resume them; this also stops their grace timers.
	for _, c := range a.liveConns() {
		c.end()
	}

	a.startMu.Lock()
	stop := a.stop
	a.startMu.Unlock()
	if stop != nil {
		stop()
		done := make(chan struct{})
		go func() {
			a.running.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return err
}

// goaway sends goaway on c's current socket, once per socket. Its delay
// is drawn once per session.
func (c *conn) goaway() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sock == nil || c.goawaySock == c.sock {
		return
	}
	c.goawaySock = c.sock
	if c.retryIn.Load() == 0 {
		c.retryIn.Store(1 + rand.Int64N(maxGoawayRetryMs))
	}
	c.sendLocked(&goawayMsg{T: "goaway", Reason: "restart", RetryInMs: c.retryIn.Load()})
}

// drainCalls waits until no call is in flight on any session, ended ones
// included, or ctx is done. New calls are refused once shutting is set, so
// the count only falls.
func (a *App) drainCalls(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if a.calls.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (a *App) addLive(c *conn) {
	a.liveMu.Lock()
	a.live[c] = struct{}{}
	a.liveMu.Unlock()
}

func (a *App) dropLive(c *conn) {
	a.liveMu.Lock()
	delete(a.live, c)
	a.liveMu.Unlock()
}

func (a *App) liveConns() []*conn {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	out := make([]*conn, 0, len(a.live))
	for c := range a.live {
		out = append(out, c)
	}
	return out
}
