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

// capResume is the welcome/hello cap for session resume (§7.2).
const capResume = "resume"

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
	s := &socket{ws: ws, outSig: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	a.serve(s, cookieIdent, cookieErr)
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

// conn is a session: subscriptions, identity, seq and the replay buffer.
// It is attached to at most one socket at a time and, when resume is on,
// outlives a lost socket for Config.ResumeGrace (§7.2).
type conn struct {
	a       *App
	session string
	resume  bool // client and server both have the resume cap
	evt     bool // the client offered the evt cap: send it evt messages
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex // guards the fields below
	sock     *socket    // nil while detached
	seq      uint64
	closed   bool // session ended; nothing is sent any more
	ident    *Identity
	expTimer *time.Timer
	grace    *time.Timer

	// Replay buffer (resume only): stateful messages after acked. floor is
	// the highest seq dropped by overflow while attached; resuming from
	// below it is impossible.
	replay      []replayMsg
	replayBytes int
	acked       uint64
	floor       uint64
	// ids maps call/auth ids seen to the seq of their res (0 while running),
	// to deduplicate re-sent calls after resume. Pruned by ack.
	ids map[uint64]uint64

	authMu   sync.Mutex // serializes identity changes
	inflight atomic.Int32
}

type replayMsg struct {
	seq uint64
	b   []byte
}

// socket is one WebSocket. Its writer drains out; a lost socket leaves the
// session behind.
type socket struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	outSig chan struct{}

	mu       sync.Mutex // guards the fields below
	out      [][]byte
	outBytes int
	dead     bool
	fatal    bool // closed for a reason that also ends the session (protocol error, deliberate close)
}

func (a *App) serve(s *socket, cookieIdent *Identity, cookieErr error) {
	defer s.ws.CloseNow()
	defer s.cancel()
	s.ws.SetReadLimit(int64(a.cfg.MaxMsg))
	go s.writer()
	go s.heartbeat(a.cfg.Heartbeat)

	first, ok := s.read()
	if !ok {
		return
	}
	if first.T != "hello" {
		s.fail(closeProtocol, "PROTOCOL", "first message must be hello")
		return
	}
	c := a.hello(s, first, cookieIdent, cookieErr)
	if c == nil {
		return
	}
	defer c.socketLost(s)
	for {
		msg, ok := s.read()
		if !ok {
			return
		}
		c.dispatch(s, msg)
	}
}

func (s *socket) read() (*clientMsg, bool) {
	typ, data, err := s.ws.Read(s.ctx)
	if err != nil {
		// A deliberate close (Comet.close, page unload) will not resume.
		if st := websocket.CloseStatus(err); st == websocket.StatusNormalClosure || st == websocket.StatusGoingAway {
			s.mu.Lock()
			s.fatal = true
			s.mu.Unlock()
		}
		return nil, false
	}
	var msg clientMsg
	if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil || msg.T == "" {
		s.fail(closeProtocol, "PROTOCOL", "malformed message")
		return nil, false
	}
	return &msg, true
}

// fail closes the socket with a protocol close code and JSON reason.
// PROTOCOL and UNAUTHENTICATED also end the session; the others (heartbeat,
// slow consumer, going away) leave it resumable. It never blocks the caller.
func (s *socket) fail(code websocket.StatusCode, name, msg string) {
	s.mu.Lock()
	s.dead = true
	s.out, s.outBytes = nil, 0
	if code == closeProtocol || code == closeUnauth {
		s.fatal = true
	}
	s.mu.Unlock()
	reason, _ := json.Marshal(map[string]string{"code": name, "msg": msg})
	if len(reason) > 123 {
		reason, _ = json.Marshal(map[string]string{"code": name})
	}
	go func() {
		_ = s.ws.Close(code, string(reason))
		s.cancel()
	}()
}

func (s *socket) isFatal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// socketLost detaches s. Without resume the session ends now; with it, the
// session waits ResumeGrace for a hello.resume.
func (c *conn) socketLost(s *socket) {
	fatal := s.isFatal()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sock != s {
		return // already replaced by a resumed socket
	}
	c.sock = nil
	if c.closed {
		return
	}
	if !c.resume || fatal {
		c.endLocked()
		return
	}
	var t *time.Timer
	t = time.AfterFunc(c.a.cfg.ResumeGrace, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.grace == t && c.sock == nil && !c.closed {
			c.endLocked()
		}
	})
	c.grace = t
}

// endLocked ends the session: its subs are unbound and it can no longer be
// resumed. Caller holds c.mu.
func (c *conn) endLocked() {
	if c.closed {
		return
	}
	c.closed = true
	if c.expTimer != nil {
		c.expTimer.Stop()
	}
	if c.grace != nil {
		c.grace.Stop()
		c.grace = nil
	}
	c.replay, c.replayBytes, c.ids = nil, 0, nil
	if c.sock != nil {
		// Its client would otherwise sit on a silent socket; closing it
		// makes the client reconnect and resync.
		c.sock.fail(closeGoingAway, CodeUnavailable, "session ended")
	}
	c.cancel()
	c.a.engine.enqueue(closeReq{c: c})
	c.a.dropSession(c)
}

func (c *conn) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked()
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
// It also runs while the session is detached; the result lands in replay.
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

// samePrincipal reports whether a and b are the same user (or both anonymous).
func samePrincipal(a, b *Identity) bool {
	return (a == nil) == (b == nil) && a.userID() == b.userID()
}

// hello authenticates the socket and attaches it to a resumed or new
// session. It returns nil if the socket was closed.
func (a *App) hello(s *socket, msg *clientMsg, cookieIdent *Identity, cookieErr error) *conn {
	if msg.V != 1 {
		s.fail(closeProtocol, "PROTOCOL", fmt.Sprintf("unsupported version %d", msg.V))
		return nil
	}
	var ident *Identity
	var authErr *Error
	switch {
	case msg.Auth != nil:
		ident, authErr = a.authenticate(s.ctx, msg.Auth.Token)
	case cookieErr != nil:
		s.fail(closeUnauth, CodeUnauthenticated, "invalid session")
		return nil
	default:
		ident = cookieIdent
	}
	resume := a.cfg.ResumeGrace > 0 && slices.Contains(msg.Caps, capResume)
	if resume && msg.Resume != nil {
		if c := a.resumeSession(s, msg.Resume, ident, authErr); c != nil {
			return c
		}
	}

	c := &conn{a: a, session: "s_" + newID(9), resume: resume, evt: slices.Contains(msg.Caps, capEvt)}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	if resume {
		c.ids = map[uint64]uint64{}
		a.addSession(c)
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.setIdentity(ident)
	c.mu.Lock()
	c.sock = s
	c.mu.Unlock()
	c.send(a.welcome(c, ident, false))
	if authErr != nil {
		c.send(&authResMsg{T: "authRes", OK: false, Err: authErr})
	}
	return c
}

func (a *App) welcome(c *conn, ident *Identity, resumed bool) *welcomeMsg {
	cfg := a.cfg
	caps := []string{capEvt}
	if cfg.ResumeGrace > 0 {
		caps = append(caps, capResume)
	}
	return &welcomeMsg{
		T: "welcome", Session: c.session, Epoch: a.epoch, Resumed: resumed,
		HB: cfg.Heartbeat.Milliseconds(), User: ident.wireUser(), AuthExp: ident.authExp(),
		Limits: limitsMsg{MaxSubs: cfg.MaxSubs, MaxInflight: cfg.MaxInflight, MaxMsg: cfg.MaxMsg},
		Build:  cfg.Build, Caps: caps,
	}
}

// resumeSession attaches s to the session r names, replaying what the client
// missed (§7.2). It returns nil when resume is impossible; the caller then
// starts a new session and the client resyncs (§7.1). A failed resume ends
// the old session: its owner has given up on it.
func (a *App) resumeSession(s *socket, r *helloResume, ident *Identity, authErr *Error) *conn {
	c := a.lookupSession(r.Session)
	if c == nil {
		return nil
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	// A different principal (login, logout or expiry during the gap) cannot
	// take over the session's subscriptions; it resyncs under its own identity.
	if !samePrincipal(c.identity(), ident) {
		c.end()
		return nil
	}
	welcome, err := json.Marshal(a.welcome(c, ident, true))
	if err != nil {
		return nil
	}

	c.mu.Lock()
	if c.closed || r.Seq > c.seq || r.Seq < c.acked || r.Seq < c.floor {
		c.endLocked()
		c.mu.Unlock()
		return nil
	}
	if old := c.sock; old != nil {
		// The client saw the old socket die before the server did.
		old.fail(closeGoingAway, CodeUnavailable, "session resumed on another connection")
	}
	if c.grace != nil {
		c.grace.Stop()
		c.grace = nil
	}
	c.sock = s
	// welcome, then the replay, then anything new: all under c.mu, so
	// nothing interleaves.
	s.push(welcome)
	for _, m := range c.replay {
		if m.seq > r.Seq {
			s.push(m.b)
		}
	}
	c.ackLocked(r.Seq)
	c.mu.Unlock()

	c.setIdentity(ident) // same principal; picks up a refreshed expiry
	keep := make(map[uint64]bool, len(r.Subs))
	for _, sid := range r.Subs {
		keep[sid] = true
	}
	a.engine.enqueue(resumeReq{c: c, keep: keep})
	if authErr != nil {
		c.send(&authResMsg{T: "authRes", OK: false, Err: authErr})
	}
	return c
}

func (a *App) addSession(c *conn) {
	a.sessMu.Lock()
	a.sessions[c.session] = c
	a.sessMu.Unlock()
}

func (a *App) lookupSession(id string) *conn {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	return a.sessions[id]
}

func (a *App) dropSession(c *conn) {
	a.sessMu.Lock()
	if a.sessions[c.session] == c {
		delete(a.sessions, c.session)
	}
	a.sessMu.Unlock()
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

func (c *conn) dispatch(s *socket, msg *clientMsg) {
	c.mu.Lock()
	current := c.sock == s
	c.mu.Unlock()
	if !current {
		return // a socket replaced by resume; its client has moved on
	}
	switch msg.T {
	case "sub":
		if msg.S == nil || msg.Name == "" {
			s.fail(closeProtocol, "PROTOCOL", "sub requires s and name")
			return
		}
		if len(msg.Args) > 64<<10 {
			c.sendTx([]subEntry{{S: *msg.S, End: NewError(CodeBadRequest, "args too large")}})
			return
		}
		c.a.engine.enqueue(subReq{c: c, s: *msg.S, name: msg.Name, args: msg.Args, have: msg.Have, ident: c.identity()})
	case "unsub":
		if msg.S == nil {
			s.fail(closeProtocol, "PROTOCOL", "unsub requires s")
			return
		}
		c.a.engine.enqueue(unsubReq{c: c, s: *msg.S})
	case "call":
		if msg.ID == nil || msg.M == "" {
			s.fail(closeProtocol, "PROTOCOL", "call requires id and m")
			return
		}
		if !c.firstSeen(*msg.ID) {
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
			s.fail(closeProtocol, "PROTOCOL", "auth requires id")
			return
		}
		if !c.firstSeen(*msg.ID) {
			return
		}
		go c.reauth(*msg.ID, msg.Token)
	case "ack":
		if msg.Seq != nil {
			c.mu.Lock()
			c.ackLocked(*msg.Seq)
			c.mu.Unlock()
		}
	case "ping":
		c.send(&pongMsg{T: "pong", N: msg.N})
	case "pong":
		// Liveness only.
	case "hello":
		s.fail(closeProtocol, "PROTOCOL", "duplicate hello")
	default:
		// Unknown types are ignored for forward compatibility.
	}
}

// firstSeen records a call/auth id and reports whether it is new. After a
// resume the client re-sends unanswered calls with their original ids: one
// still running is ignored, and a finished one's res is already in the
// replay (§7.2). Without resume every id is new.
func (c *conn) firstSeen(id uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		return true
	}
	if _, seen := c.ids[id]; seen {
		return false
	}
	c.ids[id] = 0
	return true
}

// ackLocked drops replayed messages and finished ids up to seq. Acks for
// seqs never sent are ignored. Caller holds c.mu.
func (c *conn) ackLocked(seq uint64) {
	if !c.resume || seq <= c.acked || seq > c.seq {
		return
	}
	c.acked = seq
	i := 0
	for i < len(c.replay) && c.replay[i].seq <= seq {
		c.replayBytes -= len(c.replay[i].b)
		i++
	}
	c.replay = slices.Delete(c.replay, 0, i)
	for id, rs := range c.ids {
		if rs != 0 && rs <= seq {
			delete(c.ids, id)
		}
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
	if samePrincipal(old, ident) {
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
	// The method outlives a dropped connection: its outcome is decided
	// server-side either way, and the client reports CONNECTION_LOST.
	ctx := context.WithoutCancel(c.ctx)
	tx, err := c.a.db.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(context.Background())

	result, err := func() (r any, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return m.run(&Call{Context: ctx, Identity: ident, Tx: tx, app: c.a}, msg.Args)
	}()
	if err != nil {
		return nil, false, err
	}
	if val, err = json.Marshal(result); err != nil {
		return nil, false, err
	}

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
	var seq uint64
	if s, ok := m.(sequenced); ok {
		c.seq++
		seq = c.seq
		s.setSeq(seq)
	}
	b, err := json.Marshal(m)
	if err != nil {
		c.a.log.Error("marshal outbound message", "err", err)
		return
	}
	if seq != 0 && c.resume {
		switch m := m.(type) {
		case *resMsg:
			c.ids[m.ID] = seq
		case *authResMsg:
			if m.ID != nil {
				c.ids[*m.ID] = seq
			}
		}
		if !c.buffer(seq, b) {
			return
		}
	}
	if c.sock != nil {
		c.sock.push(b)
	}
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
		b, err := json.Marshal(&txMsg{T: "tx", Seq: c.seq, Subs: p, More: i < len(parts)-1})
		if err != nil {
			c.a.log.Error("marshal tx", "err", err)
			return
		}
		if c.resume && !c.buffer(c.seq, b) {
			return
		}
		if c.sock != nil {
			c.sock.push(b)
		}
	}
}

// buffer appends a stateful message to the replay buffer. Over
// ResumeBuffer, a detached session ends (it can no longer be resumed and
// would only grow); an attached one drops its oldest messages and raises
// floor, so resume works again once the client acks past them. It reports
// false if the session ended. Caller holds c.mu.
func (c *conn) buffer(seq uint64, b []byte) bool {
	c.replay = append(c.replay, replayMsg{seq: seq, b: b})
	c.replayBytes += len(b)
	if c.replayBytes <= c.a.cfg.ResumeBuffer {
		return true
	}
	if c.sock == nil {
		c.endLocked()
		return false
	}
	i := 0
	for i < len(c.replay) && c.replayBytes > c.a.cfg.ResumeBuffer {
		c.replayBytes -= len(c.replay[i].b)
		c.floor = max(c.floor, c.replay[i].seq)
		i++
	}
	c.replay = slices.Delete(c.replay, 0, i)
	// A res below floor can no longer be replayed, so its id need not be
	// remembered either; this bounds ids for a client that never acks.
	for id, rs := range c.ids {
		if rs != 0 && rs <= c.floor {
			delete(c.ids, id)
		}
	}
	return true
}

// push appends to the socket's outbound queue.
func (s *socket) push(b []byte) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.out = append(s.out, b)
	s.outBytes += len(b)
	over := s.outBytes > maxQueuedBytes
	s.mu.Unlock()
	if over {
		s.fail(closeSlow, "SLOW_CONSUMER", "client is not keeping up")
		return
	}
	select {
	case s.outSig <- struct{}{}:
	default:
	}
}

func (s *socket) writer() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.outSig:
		}
		s.mu.Lock()
		batch := s.out
		s.out = nil
		s.mu.Unlock()
		for _, b := range batch {
			ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
			err := s.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			s.mu.Lock()
			s.outBytes -= len(b)
			s.mu.Unlock()
			if err != nil {
				if s.ctx.Err() == nil {
					s.fail(closeSlow, "SLOW_CONSUMER", "write timed out")
				}
				return
			}
		}
	}
}

func (s *socket) heartbeat(hb time.Duration) {
	t := time.NewTicker(hb)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 2*hb)
		err := s.ws.Ping(ctx)
		cancel()
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail(closeHeartbeat, "HEARTBEAT_TIMEOUT", "no pong")
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
