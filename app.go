// Package cometgo is a realtime web framework: publish Postgres queries as
// live subscriptions and expose server-authoritative methods over a WebSocket
// wire protocol (see PROTOCOL.md).
package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Identity is an authenticated principal.
type Identity struct {
	// UserID identifies the user. Publications with ScopeUser are shared only
	// between connections with the same Scope().
	UserID string
	// ScopeKey, if set, partitions one user's identities, e.g. by tenant: two
	// identities with the same UserID but different ScopeKey get separate
	// ScopeUser observers and Emit targets, and are different principals for
	// resume, re-auth and idempotency keys. Set it when the query depends on
	// more than the user. It is always combined with UserID (see Scope).
	ScopeKey string
	// User is sent to the client in welcome/authRes. Defaults to {"id": UserID}.
	User any
	// ExpiresAt, if set, downgrades the connection to anonymous when reached
	// unless the client refreshes its token first.
	ExpiresAt time.Time
	// Data is free-form server-side data available to methods and publications.
	Data any
}

func (id *Identity) userID() string {
	if id == nil {
		return ""
	}
	return id.UserID
}

// validate rejects a UserID or ScopeKey that Postgres text cannot store
// (idempotency rows, revalidate payloads) or that would break Scope's NUL
// separator. nil (anonymous) is valid.
func (id *Identity) validate() error {
	if id == nil {
		return nil
	}
	for _, f := range [...]struct{ name, v string }{{"UserID", id.UserID}, {"ScopeKey", id.ScopeKey}} {
		if strings.IndexByte(f.v, 0) >= 0 {
			return fmt.Errorf("Identity.%s contains a NUL byte", f.name)
		}
		if !utf8.ValidString(f.v) {
			return fmt.Errorf("Identity.%s is not valid UTF-8", f.name)
		}
	}
	return nil
}

// Scope is the identity's scope: what ScopeUser observers are keyed by and
// what Emit's scope argument must be. It is UserID when ScopeKey is empty,
// otherwise UserID and ScopeKey joined by a NUL byte, so it never equals
// another user's scope as long as UserIDs contain no NUL. "" when nil.
func (id *Identity) Scope() string {
	if id == nil {
		return ""
	}
	if id.ScopeKey == "" {
		return id.UserID
	}
	return id.UserID + "\x00" + id.ScopeKey
}

// ikMethod is the method recorded with an idempotency key. Keys are stored
// per UserID (so rows written before ScopeKey was set still match); the
// ScopeKey rides along so a key reused under another scope is refused rather
// than run again or answered with the other scope's result.
func ikMethod(method string, id *Identity) string {
	if id == nil || id.ScopeKey == "" {
		return method
	}
	return method + "\x1f" + id.ScopeKey // Postgres text cannot hold NUL
}

func (id *Identity) wireUser() any {
	switch {
	case id == nil:
		return nil
	case id.User != nil:
		return id.User
	default:
		return map[string]string{"id": id.UserID}
	}
}

func (id *Identity) authExp() int64 {
	if id == nil || id.ExpiresAt.IsZero() {
		return 0
	}
	return id.ExpiresAt.Unix()
}

// Config configures an App.
type Config struct {
	// DB is the Postgres pool used for publications, methods and LISTEN.
	DB *pgxpool.Pool

	// Authenticate validates a bearer token sent in hello or auth. Return an
	// *Error (e.g. UNAUTHENTICATED) to reject it. If nil, tokens are rejected.
	Authenticate func(ctx context.Context, token string) (*Identity, error)

	// AuthenticateRequest resolves cookie-session identity at WebSocket
	// upgrade, used when hello carries no token. Return (nil, nil) for
	// anonymous; an error closes the connection with 4401.
	AuthenticateRequest func(r *http.Request) (*Identity, error)

	// Build identifies the current client build; sent in welcome (§3.1).
	Build string

	// OriginPatterns lists extra allowed WebSocket origins (host patterns).
	// Same-origin is always allowed.
	OriginPatterns []string

	// Heartbeat is the liveness interval. Default 25s.
	Heartbeat time.Duration

	// MinTickInterval is the minimum time between ticks, coalescing bursts
	// of changes. Default 20ms.
	MinTickInterval time.Duration

	// FenceTimeout bounds how long a method result waits for its writes to
	// reach the client before being sent with lag:true. Default 5s.
	FenceTimeout time.Duration

	// QueryTimeout is the hard per-observer query timeout. Default 30s.
	QueryTimeout time.Duration

	// IdempotencyTTL is how long the result of a call carrying an
	// idempotency key is kept for retries. Default 24h.
	IdempotencyTTL time.Duration

	MaxSubs     int // default 500
	MaxInflight int // default 32
	MaxMsg      int // default 1 MiB
	MaxRows     int // per-observer row cap, default 10,000

	// MaxObservers caps live queries (observers) on the server. A sub that
	// would create one more ends with OVERLOADED; a sub joining an existing
	// observer is unaffected. 0 means no limit.
	MaxObservers int
	// MaxConns caps open WebSocket connections; MaxConnsPerIP caps them per
	// ClientIP. Over either, the upgrade is refused with HTTP 503. 0 means
	// no limit.
	MaxConns      int
	MaxConnsPerIP int
	// ClientIP returns the address MaxConnsPerIP counts against. Default,
	// and when it returns "": the host of r.RemoteAddr. Behind a proxy,
	// return the client address the proxy forwards, or every client shares
	// the proxy's limit.
	ClientIP func(r *http.Request) string

	// ResumeGrace is how long a session outlives a lost socket, waiting for
	// the client to resume it (§7.2). Default 60s; negative disables resume.
	// Resume needs sticky routing when several nodes serve one endpoint.
	ResumeGrace time.Duration

	// ResumeBuffer caps the bytes of unacked messages kept per session for
	// replay. A detached session that overflows it ends. Default 1 MiB, at
	// most 4 MiB.
	ResumeBuffer int

	Logger *slog.Logger

	// Metrics, if set, receives engine and connection health measurements
	// (see the Metric* constants). It is called synchronously from the
	// engine and connection goroutines, so it must be fast and safe for
	// concurrent use. Nil reports nothing.
	Metrics func(Metric)
}

// App is a CometGo server. It implements http.Handler for the WebSocket
// endpoint; mount it at /_comet.
type App struct {
	cfg     Config
	db      *pgxpool.Pool
	log     *slog.Logger
	epoch   string
	nodeID  string
	pubs    map[string]*publication
	methods map[string]*method
	engine  *engine
	started bool

	sessMu   sync.Mutex
	sessions map[string]*conn // resumable sessions, by id

	// Graceful shutdown (§7.3).
	shutting atomic.Bool
	calls    atomic.Int64 // calls in flight on every session, ended ones too
	liveMu   sync.Mutex
	live     map[*conn]struct{} // every session not yet ended
	shutOnce sync.Once
	shutDone chan struct{}
	shutErr  error
	connMu   sync.Mutex         // guards conns and ipConns
	conns    int                // open WebSocket connections
	ipConns  map[string]int     // open connections by ClientIP
	startMu  sync.Mutex         // guards started and stop
	stop     context.CancelFunc // stops what Start runs
	running  sync.WaitGroup
}

// New creates an App. Register publications and methods, then call Start.
func New(cfg Config) *App {
	if cfg.DB == nil {
		panic("cometgo: Config.DB is required")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	defi := func(d *int, v int) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Heartbeat, 25*time.Second)
	def(&cfg.MinTickInterval, 20*time.Millisecond)
	def(&cfg.FenceTimeout, 5*time.Second)
	def(&cfg.QueryTimeout, 30*time.Second)
	def(&cfg.IdempotencyTTL, 24*time.Hour)
	defi(&cfg.MaxSubs, 500)
	defi(&cfg.MaxInflight, 32)
	defi(&cfg.MaxMsg, 1<<20)
	defi(&cfg.MaxRows, 10_000)
	if cfg.ResumeGrace == 0 {
		cfg.ResumeGrace = 60 * time.Second
	}
	defi(&cfg.ResumeBuffer, 1<<20)
	// A resume pushes the whole replay at once; it must fit the outbound
	// queue or every resume would close as SLOW_CONSUMER.
	cfg.ResumeBuffer = min(cfg.ResumeBuffer, maxQueuedBytes/2)
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	a := &App{
		cfg:      cfg,
		db:       cfg.DB,
		log:      cfg.Logger.With("component", "cometgo"),
		epoch:    "b_" + newID(8),
		nodeID:   newID(6),
		pubs:     map[string]*publication{},
		methods:  map[string]*method{},
		sessions: map[string]*conn{},
		live:     map[*conn]struct{}{},
		ipConns:  map[string]int{},
		shutDone: make(chan struct{}),
	}
	a.engine = newEngine(a)
	return a
}

// Start runs the Postgres listener, the tick engine and the idempotency-key
// cleanup until ctx is done or Shutdown stops them. Shutdown needs them
// running while it drains, so with Shutdown pass a ctx that outlives the
// shutdown signal. After Shutdown, Start does nothing.
//
// Start first checks that every table in a publication's Tables exists under
// exactly that name and has the triggers InstallTriggers creates, and returns
// an error without starting anything if not. The check needs the database:
// if it is unreachable, Start fails after at most Config.QueryTimeout. A
// later Start may retry.
func (a *App) Start(ctx context.Context) error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if a.started {
		panic("cometgo: Start called twice")
	}
	if a.shutting.Load() {
		a.started = true
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, a.cfg.QueryTimeout)
	defer cancel()
	if err := a.checkTables(checkCtx); err != nil {
		// Logged too: a caller that ignores the error would otherwise serve
		// an app with no engine, and no clue why.
		a.log.Error("start failed", "err", err)
		return err
	}
	a.started = true
	ctx, a.stop = context.WithCancel(ctx)
	for _, run := range []func(context.Context){a.engine.listen, a.engine.run, a.cleanIdempotency} {
		a.running.Add(1)
		go func() {
			defer a.running.Done()
			run(ctx)
		}()
	}
	return nil
}

// Scope controls which connections share one observer of a publication.
type Scope int

const (
	// ScopeUser shares an observer only between connections of the same user.
	// This is the safe default for any query that depends on identity.
	ScopeUser Scope = iota
	// ScopeShared shares one observer between all connections. Only use it
	// when the query does not depend on the caller's identity.
	ScopeShared
)

// PubOptions configures a publication.
type PubOptions struct {
	// Tables the query reads ("todos" or "schema.todos"). Changes to these
	// tables re-run the query. Each must have triggers installed with
	// InstallTriggers.
	Tables []string
	// Key lists the primary-key columns of the result. Default ["id"].
	Key []string
	// Unordered marks results whose row order carries no meaning; the client
	// then receives no positions.
	Unordered bool
	Scope     Scope
	// Events lets the server send ephemeral events to this publication's
	// subscriptions with App.Emit and Call.EmitAfterCommit.
	Events bool
	// Row, if set, is a struct value describing one result row for
	// Describe, which then does not run the query. Use it for a query that
	// cannot run with zero-value args. Fields map to columns by their json
	// tag or name; a pointer field is nullable (see Describe).
	Row any
}

// Query is the SQL a publication runs. It is re-run whenever a table it
// reads changes, so it must be deterministic for a given snapshot; use
// ORDER BY for ordered publications.
type Query struct {
	SQL  string
	Args []any

	// Match, if set, lets the engine skip re-running this query for writes
	// that cannot affect it (PROTOCOL.md §11). It must describe the query's
	// row set exactly: the publication reads one table, Key is that table's
	// primary key (same columns, same order), and the result is every row
	// of the table where each column equals its value, with no LIMIT,
	// OFFSET, join, subquery or other condition. An empty map means every
	// row. Values are compared as pgx decodes the column: string for text
	// and enums (deterministic collations only), int types for integers,
	// bool, and [16]byte or its string form for uuid. Anything else, a nil
	// value, or a column the table lacks, makes every change re-run the
	// query, as without Match.
	Match map[string]any
}

// SubCtx is passed to publication query builders.
type SubCtx struct {
	Context  context.Context
	Identity *Identity // nil when anonymous
}

type publication struct {
	name    string
	opts    PubOptions
	tables  []string
	build   func(c *SubCtx, args json.RawMessage) (Query, error)
	argType reflect.Type // for Describe
}

// Publish registers a publication. Args are decoded from the client's sub
// args into A; a decode failure ends the sub with BAD_REQUEST.
func Publish[A any](a *App, name string, opts PubOptions, fn func(c *SubCtx, args A) (Query, error)) {
	if _, dup := a.pubs[name]; dup {
		panic("cometgo: duplicate publication " + name)
	}
	if len(opts.Tables) == 0 {
		panic("cometgo: publication " + name + " declares no Tables")
	}
	if len(opts.Key) == 0 {
		opts.Key = []string{"id"}
	}
	p := &publication{name: name, opts: opts, argType: reflect.TypeFor[A]()}
	for _, t := range opts.Tables {
		p.tables = append(p.tables, qualifyTable(t))
	}
	p.build = func(c *SubCtx, raw json.RawMessage) (Query, error) {
		var args A
		if err := decodeArgs(raw, &args); err != nil {
			return Query{}, err
		}
		return fn(c, args)
	}
	a.pubs[name] = p
}

// Call is passed to method handlers.
type Call struct {
	Context  context.Context
	Identity *Identity // nil when anonymous
	// Tx is the method's transaction. Writes made through it are fenced:
	// the client's call resolves only after their effects reach its subs.
	Tx pgx.Tx

	app *App
}

type method struct {
	name             string
	run              func(c *Call, args json.RawMessage) (any, error)
	argType, resType reflect.Type // for Describe
}

// Method registers a server method. Args are decoded into A; the handler's
// result R is JSON-encoded into res.val. Returning an error rolls back Tx.
func Method[A, R any](a *App, name string, fn func(c *Call, args A) (R, error)) {
	if _, dup := a.methods[name]; dup {
		panic("cometgo: duplicate method " + name)
	}
	a.methods[name] = &method{name: name, argType: reflect.TypeFor[A](), resType: reflect.TypeFor[R](), run: func(c *Call, raw json.RawMessage) (any, error) {
		var args A
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		return fn(c, args)
	}}
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) || strings.HasPrefix(err.Error(), "json: unknown field") {
			return &Error{Code: CodeBadRequest, Msg: "invalid args: " + err.Error()}
		}
		return &Error{Code: CodeBadRequest, Msg: "invalid args"}
	}
	return nil
}

func qualifyTable(t string) string {
	if strings.Contains(t, ".") {
		return t
	}
	return "public." + t
}

// canonicalJSON re-encodes raw so equal values have equal bytes (object keys
// sorted, whitespace removed).
func canonicalJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "null", nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("args: %w", err)
	}
	b, err := json.Marshal(v)
	return string(b), err
}
