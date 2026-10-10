package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Close codes (PROTOCOL.md §9).
const (
	closeProtocol  websocket.StatusCode = 4400
	closeUnauth    websocket.StatusCode = 4401
	closeHeartbeat websocket.StatusCode = 4408
	closeSlow      websocket.StatusCode = 4429
	closeGoingAway websocket.StatusCode = 4503
)

const (
	maxQueuedBytes = 8 << 20   // §8.1 hard limit
	frameTarget    = 256 << 10 // §4.3 split target
	writeTimeout   = 10 * time.Second
)

// ServeHTTP upgrades the request to a CometGo WebSocket connection.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !offersSubprotocol(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{"supported": []string{Subprotocol}})
		return
	}
	var cookieIdent *Identity
	var cookieErr error
	if a.cfg.AuthenticateRequest != nil {
		cookieIdent, cookieErr = a.cfg.AuthenticateRequest(r)
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{Subprotocol},
		OriginPatterns: a.cfg.OriginPatterns,
	})
	if err != nil {
		return // Accept already wrote the HTTP error (e.g. bad Origin)
	}
	c := &conn{
		a:           a,
		ws:          ws,
		session:     "s_" + newID(9),
		outSig:      make(chan struct{}, 1),
		cookieIdent: cookieIdent,
		cookieErr:   cookieErr,
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.serve()
}

func offersSubprotocol(r *http.Request) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			if strings.TrimSpace(p) == Subprotocol {
				return true
			}
		}
	}
	return false
}

type conn struct {
	a       *App
	ws      *websocket.Conn
	session string
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex // guards the fields below
	out      [][]byte
	outBytes int
	seq      uint64
	closed   bool
	ident    *Identity
	expTimer *time.Timer

	outSig      chan struct{}
	authMu      sync.Mutex // serializes identity changes
	inflight    atomic.Int32
	cookieIdent *Identity
	cookieErr   error
}

func (c *conn) serve() {
	defer c.shutdown()
	c.ws.SetReadLimit(int64(c.a.cfg.MaxMsg))
	go c.writer()
	go c.heartbeat()

	first, ok := c.read()
	if !ok {
		return
	}
	if first.T != "hello" {
		c.fail(closeProtocol, "PROTOCOL", "first message must be hello")
		return
	}
	if !c.hello(first) {
		return
	}
	for {
		msg, ok := c.read()
		if !ok {
			return
		}
		c.dispatch(msg)
	}
}

func (c *conn) read() (*clientMsg, bool) {
	typ, data, err := c.ws.Read(c.ctx)
	if err != nil {
		return nil, false
	}
	var msg clientMsg
	if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil || msg.T == "" {
		c.fail(closeProtocol, "PROTOCOL", "malformed message")
		return nil, false
	}
	return &msg, true
}

func (c *conn) shutdown() {
	c.mu.Lock()
	c.closed = true
	if c.expTimer != nil {
		c.expTimer.Stop()
	}
	c.mu.Unlock()
	c.cancel()
	c.a.engine.enqueue(closeReq{c: c})
	_ = c.ws.CloseNow()
}

// fail closes the connection with a protocol close code and JSON reason.
// It never blocks the caller.
func (c *conn) fail(code websocket.StatusCode, name, msg string) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	reason, _ := json.Marshal(map[string]string{"code": name, "msg": msg})
	if len(reason) > 123 {
		reason, _ = json.Marshal(map[string]string{"code": name})
	}
	go func() {
		_ = c.ws.Close(code, string(reason))
		c.cancel()
	}()
}

func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *conn) identity() *Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ident
}

// setIdentity installs ident and arms its expiry timer. Callers hold authMu.
func (c *conn) setIdentity(ident *Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ident = ident
	if c.expTimer != nil {
		c.expTimer.Stop()
		c.expTimer = nil
	}
	if ident != nil && !ident.ExpiresAt.IsZero() && !c.closed {
		c.expTimer = time.AfterFunc(time.Until(ident.ExpiresAt), func() { c.expire(ident) })
	}
}

// expire downgrades to anonymous when a token lapses without refresh (§3.2).
func (c *conn) expire(ident *Identity) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.identity() != ident {
		return // refreshed in the meantime
	}
	c.setIdentity(nil)
	c.a.engine.enqueue(rebindReq{c: c, ident: nil, after: &authResMsg{
		T: "authRes", OK: false, Err: NewError(CodeAuthExpired, "token expired"),
	}})
}

func (c *conn) hello(msg *clientMsg) bool {
	if msg.V != 1 {
		c.fail(closeProtocol, "PROTOCOL", fmt.Sprintf("unsupported version %d", msg.V))
		return false
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	var ident *Identity
	var authErr *Error
	switch {
	case msg.Auth != nil:
		ident, authErr = c.a.authenticate(c.ctx, msg.Auth.Token)
	case c.cookieErr != nil:
		c.fail(closeUnauth, CodeUnauthenticated, "invalid session")
		return false
	default:
		ident = c.cookieIdent
	}
	c.setIdentity(ident)
	cfg := c.a.cfg
	// The server honours ik whether or not it was negotiated; the cap tells
	// the client it may re-send ik calls after a disconnect.
	caps := []string{}
	if slices.Contains(msg.Caps, capIK) {
		caps = append(caps, capIK)
	}
	c.send(&welcomeMsg{
		T: "welcome", Session: c.session, Epoch: c.a.epoch,
		HB: cfg.Heartbeat.Milliseconds(), User: ident.wireUser(), AuthExp: ident.authExp(),
		Limits: limitsMsg{MaxSubs: cfg.MaxSubs, MaxInflight: cfg.MaxInflight, MaxMsg: cfg.MaxMsg},
		Build:  cfg.Build, Caps: caps,
	})
	if authErr != nil {
		c.send(&authResMsg{T: "authRes", OK: false, Err: authErr})
	}
	return true
}

func (a *App) authenticate(ctx context.Context, token string) (*Identity, *Error) {
	if a.cfg.Authenticate == nil {
		return nil, NewError(CodeUnauthenticated, "token auth not configured")
	}
	ident, err := a.cfg.Authenticate(ctx, token)
	if err != nil {
		return nil, a.toWireError(err, "authenticate")
	}
	if ident == nil {
		return nil, NewError(CodeUnauthenticated, "invalid token")
	}
	return ident, nil
}

func (c *conn) dispatch(msg *clientMsg) {
	switch msg.T {
	case "sub":
		if msg.S == nil || msg.Name == "" {
			c.fail(closeProtocol, "PROTOCOL", "sub requires s and name")
			return
		}
		if len(msg.Args) > 64<<10 {
			c.sendTx([]subEntry{{S: *msg.S, End: NewError(CodeBadRequest, "args too large")}})
			return
		}
		c.a.engine.enqueue(subReq{c: c, s: *msg.S, name: msg.Name, args: msg.Args, have: msg.Have, ident: c.identity()})
	case "unsub":
		if msg.S == nil {
			c.fail(closeProtocol, "PROTOCOL", "unsub requires s")
			return
		}
		c.a.engine.enqueue(unsubReq{c: c, s: *msg.S})
	case "call":
		if msg.ID == nil || msg.M == "" {
			c.fail(closeProtocol, "PROTOCOL", "call requires id and m")
			return
		}
		if int(c.inflight.Add(1)) > c.a.cfg.MaxInflight {
			c.inflight.Add(-1)
			c.send(&resMsg{T: "res", ID: *msg.ID, Err: &Error{Code: CodeRateLimited, Msg: "too many calls in flight", RetryAfterMs: 100}})
			return
		}
		go c.call(msg, c.identity())
	case "auth":
		if msg.ID == nil {
			c.fail(closeProtocol, "PROTOCOL", "auth requires id")
			return
		}
		go c.reauth(*msg.ID, msg.Token)
	case "ping":
		c.send(&pongMsg{T: "pong", N: msg.N})
	case "pong", "ack":
		// Liveness only; ack is reserved for session resume.
	case "hello":
		c.fail(closeProtocol, "PROTOCOL", "duplicate hello")
	default:
		// Unknown types are ignored for forward compatibility.
	}
}

func (c *conn) reauth(id uint64, rawToken json.RawMessage) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	var ident *Identity
	var authErr *Error
	if len(rawToken) > 0 && string(rawToken) != "null" {
		var token string
		if err := json.Unmarshal(rawToken, &token); err != nil {
			authErr = NewError(CodeBadRequest, "token must be a string or null")
		} else {
			ident, authErr = c.a.authenticate(c.ctx, token)
		}
	}
	res := &authResMsg{T: "authRes", ID: &id, OK: authErr == nil, User: ident.wireUser(), AuthExp: ident.authExp(), Err: authErr}
	old := c.identity()
	c.setIdentity(ident)
	if (old == nil) == (ident == nil) && old.userID() == ident.userID() {
		// Same principal (e.g. token refresh): nothing to rebind.
		c.send(res)
		return
	}
	c.a.engine.enqueue(rebindReq{c: c, ident: ident, after: res})
}

func (c *conn) call(msg *clientMsg, ident *Identity) {
	defer c.inflight.Add(-1)
	id := *msg.ID
	val, lag, err := c.runCall(msg, ident)
	res := &resMsg{T: "res", ID: id, OK: err == nil, Val: val, Lag: lag}
	if err != nil {
		res.Err = c.a.toWireError(err, "method "+msg.M)
	}
	c.send(res)
}

func (c *conn) runCall(msg *clientMsg, ident *Identity) (val json.RawMessage, lag bool, err error) {
	m := c.a.methods[msg.M]
	if m == nil {
		return nil, false, NewError(CodeNotFound, "unknown method "+msg.M)
	}
	ik := msg.IK
	if ik != nil && (*ik == "" || len(*ik) > maxIKLen) {
		return nil, false, NewError(CodeBadRequest, "ik must be a non-empty string of at most 128 bytes")
	}
	// The method outlives a dropped connection: its outcome is decided
	// server-side either way, and the client reports CONNECTION_LOST
	// (or, for a call with ik, re-sends it and gets the stored result).
	ctx := context.WithoutCancel(c.ctx)
	tx, err := c.a.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(context.Background())

	// With ik, a stored result from an earlier call by the same user is
	// returned without running the handler. The claim row commits atomically
	// with the handler's writes, or rolls back with them on error.
	var stored json.RawMessage
	if ik != nil {
		if stored, err = claimIK(ctx, tx, *ik, ident.userID(), msg.M); err != nil {
			return nil, false, err
		}
	}
	if stored != nil {
		val = stored
	} else {
		result, err := func() (r any, err error) {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			return m.run(&Call{Context: ctx, Identity: ident, Tx: tx}, msg.Args)
		}()
		if err != nil {
			return nil, false, err
		}
		if val, err = json.Marshal(result); err != nil {
			return nil, false, err
		}
		if ik != nil {
			if err := storeIK(ctx, tx, *ik, ident.userID(), val); err != nil {
				return nil, false, err
			}
		}
	}
	// A replayed result goes through the same commit and fence as a first
	// run, so it too resolves only once this connection's subscriptions
	// reflect the original writes.

	if msg.NoFence {
		return val, false, tx.Commit(ctx)
	}
	// Fence (§6.2): registered before commit, notified as the last
	// statement, so its notification follows every row notification of
	// this transaction.
	token, f := c.a.engine.registerFence()
	if _, err := tx.Exec(ctx, "SELECT pg_notify('comet', $1)", "1|f|"+token); err != nil {
		c.a.engine.dropFence(token)
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		c.a.engine.dropFence(token)
		return nil, false, err
	}
	select {
	case <-f.released:
		return val, false, nil
	case <-time.After(c.a.cfg.FenceTimeout):
		c.a.engine.dropFence(token)
		return val, true, nil
	}
}

// send enqueues one message, assigning seq to stateful messages.
func (c *conn) send(m any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if s, ok := m.(sequenced); ok {
		c.seq++
		s.setSeq(c.seq)
	}
	b, err := json.Marshal(m)
	if err != nil {
		c.a.log.Error("marshal outbound message", "err", err)
		return
	}
	c.push(b)
}

// sendTx enqueues one tx, split into contiguous parts sharing one seq (§4.3).
func (c *conn) sendTx(entries []subEntry) {
	parts := splitTx(entries, frameTarget)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.seq++
	for i, p := range parts {
		if c.closed {
			return
		}
		b, err := json.Marshal(&txMsg{T: "tx", Seq: c.seq, Subs: p, More: i < len(parts)-1})
		if err != nil {
			c.a.log.Error("marshal tx", "err", err)
			return
		}
		c.push(b)
	}
}

// push appends to the outbound queue. Caller holds c.mu.
func (c *conn) push(b []byte) {
	c.out = append(c.out, b)
	c.outBytes += len(b)
	if c.outBytes > maxQueuedBytes {
		c.out, c.outBytes = nil, 0
		c.mu.Unlock()
		c.fail(closeSlow, "SLOW_CONSUMER", "client is not keeping up")
		c.mu.Lock()
		return
	}
	select {
	case c.outSig <- struct{}{}:
	default:
	}
}

func (c *conn) writer() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.outSig:
		}
		c.mu.Lock()
		batch := c.out
		c.out = nil
		c.mu.Unlock()
		for _, b := range batch {
			ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			c.mu.Lock()
			c.outBytes -= len(b)
			c.mu.Unlock()
			if err != nil {
				if c.ctx.Err() == nil {
					c.fail(closeSlow, "SLOW_CONSUMER", "write timed out")
				}
				return
			}
		}
	}
}

func (c *conn) heartbeat() {
	hb := c.a.cfg.Heartbeat
	t := time.NewTicker(hb)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(c.ctx, 2*hb)
		err := c.ws.Ping(ctx)
		cancel()
		if err != nil {
			if c.ctx.Err() == nil {
				c.fail(closeHeartbeat, "HEARTBEAT_TIMEOUT", "no pong")
			}
			return
		}
	}
}

// splitTx splits entries into parts of roughly target bytes. A sub may span
// parts; its init/reset travel on its first piece and ready/end on its last.
func splitTx(entries []subEntry, target int) [][]subEntry {
	const overhead = 64
	var parts [][]subEntry
	var cur []subEntry
	size := overhead
	for _, e := range entries {
		piece := subEntry{S: e.S, Init: e.Init, Reset: e.Reset}
		if e.Init != nil {
			b, _ := json.Marshal(e.Init)
			size += len(b)
		}
		size += overhead
		for _, o := range e.Ops {
			if size+len(o)+1 > target && (len(piece.Ops) > 0 || len(cur) > 0) {
				cur = append(cur, piece)
				parts = append(parts, cur)
				cur, size = nil, overhead*2
				piece = subEntry{S: e.S}
			}
			piece.Ops = append(piece.Ops, o)
			size += len(o) + 1
		}
		piece.Ready, piece.End = e.Ready, e.End
		cur = append(cur, piece)
	}
	return append(parts, slices.Clip(cur))
}
