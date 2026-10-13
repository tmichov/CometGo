package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmichov/cometgo"
)

// schema is recreated on every run, so ids start at 1 and results have the
// sizes the flags ask for.
const schema = `
DROP SCHEMA IF EXISTS cometload CASCADE;
CREATE SCHEMA cometload;
CREATE TABLE cometload.shared_items (
  id    bigserial PRIMARY KEY,
  grp   int  NOT NULL,
  title text NOT NULL,
  n     int  NOT NULL DEFAULT 0
);
CREATE INDEX ON cometload.shared_items (grp);
CREATE TABLE cometload.user_items (
  id     bigserial PRIMARY KEY,
  owner  text NOT NULL,
  bucket int  NOT NULL,
  title  text NOT NULL,
  n      int  NOT NULL DEFAULT 0
);
CREATE INDEX ON cometload.user_items (owner, bucket);
`

// setupSchema creates the tables and seeds rows per shared group and per
// user bucket.
func setupSchema(ctx context.Context, pool *pgxpool.Pool, p params) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cometload.shared_items (grp, title)
		SELECT g, 'item ' || r FROM generate_series(0, $1 - 1) g, generate_series(1, $2) r
		ORDER BY g, r`, p.groups, p.rows); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cometload.user_items (owner, bucket, title)
		SELECT 'u' || u, b, 'item ' || r FROM generate_series(0, $1 - 1) u, generate_series(0, $2 - 1) b, generate_series(1, $3) r
		ORDER BY u, b, r`, p.users, p.userSubs(), p.rows); err != nil {
		return err
	}
	return cometgo.InstallTriggers(ctx, pool, "cometload.shared_items", "cometload.user_items")
}

// serverStats collects what Config.Metrics reports. Reset starts a new
// measurement window.
type serverStats struct {
	mu               sync.Mutex
	Ticks            []float64 `json:"ticks"`
	Runs             []float64 `json:"runs"`
	FenceWait        []float64 `json:"fenceWait"`
	FenceLag         int       `json:"fenceLag"`
	SlowCloses       int       `json:"slowCloses"`
	ListenReconnects int       `json:"listenReconnects"`
	Observers        float64   `json:"observers"`
	Subscriptions    float64   `json:"subscriptions"`
	MaxQueuedBytes   float64   `json:"maxQueuedBytes"`
	MaxCoalescing    float64   `json:"maxCoalescing"`
	// Stale is stale_seconds per publication.
	Stale map[string][]float64 `json:"stale"`
}

func (s *serverStats) record(m cometgo.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch m.Name {
	case cometgo.MetricTick:
		s.Ticks = append(s.Ticks, m.Value)
	case cometgo.MetricTickRuns:
		s.Runs = append(s.Runs, m.Value)
	case cometgo.MetricFenceWait:
		s.FenceWait = append(s.FenceWait, m.Value)
	case cometgo.MetricFenceLag:
		s.FenceLag++
	case cometgo.MetricSlowClose:
		s.SlowCloses++
	case cometgo.MetricListenReconnect:
		s.ListenReconnects++
	case cometgo.MetricObservers:
		s.Observers = m.Value
	case cometgo.MetricSubscriptions:
		s.Subscriptions = m.Value
	case cometgo.MetricQueuedBytes:
		s.MaxQueuedBytes = max(s.MaxQueuedBytes, m.Value)
	case cometgo.MetricCoalescing:
		s.MaxCoalescing = max(s.MaxCoalescing, m.Value)
	case cometgo.MetricStale:
		if s.Stale == nil {
			s.Stale = map[string][]float64{}
		}
		s.Stale[m.Pub] = append(s.Stale[m.Pub], m.Value)
	}
}

func (s *serverStats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Query().Has("reset") {
		s.Ticks, s.Runs, s.FenceWait = nil, nil, nil
		s.FenceLag, s.SlowCloses, s.ListenReconnects = 0, 0, 0
		s.MaxQueuedBytes, s.MaxCoalescing = 0, 0
		s.Stale = nil
	}
	json.NewEncoder(w).Encode(s)
}

type idArgs struct {
	ID int64 `json:"id"`
}

type bucketArgs struct {
	B int `json:"b"`
}

// serve runs the CometGo server under test in this process (started by the
// driver as a child, so its RSS is the server's alone) and prints its
// address on stdout.
func serve(ctx context.Context, p params) error {
	pool, err := pgxpool.New(ctx, p.dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	stats := &serverStats{}
	app := cometgo.New(cometgo.Config{
		DB:      pool,
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Metrics: stats.record,
		Authenticate: func(ctx context.Context, token string) (*cometgo.Identity, error) {
			return &cometgo.Identity{UserID: token}, nil
		},
	})

	cometgo.Publish(app, "shared.group", cometgo.PubOptions{Tables: []string{"cometload.shared_items"}, Scope: cometgo.ScopeShared},
		func(c *cometgo.SubCtx, a struct {
			G int `json:"g"`
		}) (cometgo.Query, error) {
			return cometgo.Query{
				SQL:   `SELECT id, title, n FROM cometload.shared_items WHERE grp = $1 ORDER BY id`,
				Args:  []any{a.G},
				Match: map[string]any{"grp": a.G},
			}, nil
		})
	cometgo.Publish(app, "user.bucket", cometgo.PubOptions{Tables: []string{"cometload.user_items"}},
		func(c *cometgo.SubCtx, a bucketArgs) (cometgo.Query, error) {
			if c.Identity == nil {
				return cometgo.Query{}, cometgo.ErrForbidden
			}
			return cometgo.Query{
				SQL:   `SELECT id, title, n FROM cometload.user_items WHERE owner = $1 AND bucket = $2 ORDER BY id`,
				Args:  []any{c.Identity.UserID, a.B},
				Match: map[string]any{"owner": c.Identity.UserID, "bucket": a.B},
			}, nil
		})
	// The slow observer has no Match, so every shared write re-runs it.
	cometgo.Publish(app, "slow.observer", cometgo.PubOptions{Tables: []string{"cometload.shared_items"}, Scope: cometgo.ScopeShared},
		func(c *cometgo.SubCtx, _ struct{}) (cometgo.Query, error) {
			return cometgo.Query{
				SQL:  `SELECT i.id, i.title, i.n FROM cometload.shared_items i, pg_sleep($1::float8) WHERE i.grp = 0 ORDER BY i.id`,
				Args: []any{p.slowObserver.Seconds()},
			}, nil
		})

	// The slow publication fn sleeps on the app's pool before returning its
	// Query, on every sub. Each sub has its own k, so each binds its own
	// observer; the query itself is cheap and re-runs on group g's writes.
	cometgo.Publish(app, "slow.pubfn", cometgo.PubOptions{Tables: []string{"cometload.shared_items"}, Scope: cometgo.ScopeShared},
		func(c *cometgo.SubCtx, a struct {
			G int `json:"g"`
			K int `json:"k"`
		}) (cometgo.Query, error) {
			if _, err := pool.Exec(c.Context, `SELECT pg_sleep($1::float8)`, p.slowPubFn.Seconds()); err != nil {
				return cometgo.Query{}, err
			}
			return cometgo.Query{
				SQL:   `SELECT id, title, n FROM cometload.shared_items WHERE grp = $1 ORDER BY id`,
				Args:  []any{a.G},
				Match: map[string]any{"grp": a.G},
			}, nil
		})

	cometgo.Method(app, "ping", func(c *cometgo.Call, _ struct{}) (string, error) {
		return "pong", nil
	})
	cometgo.Method(app, "shared.bump", func(c *cometgo.Call, a idArgs) (struct{}, error) {
		_, err := c.Tx.Exec(c.Context, `UPDATE cometload.shared_items SET n = n + 1 WHERE id = $1`, a.ID)
		return struct{}{}, err
	})
	cometgo.Method(app, "user.bump", func(c *cometgo.Call, a bucketArgs) (struct{}, error) {
		_, err := c.Tx.Exec(c.Context, `
			UPDATE cometload.user_items SET n = n + 1 WHERE id = (
			  SELECT id FROM cometload.user_items WHERE owner = $1 AND bucket = $2 ORDER BY random() LIMIT 1)`,
			c.Identity.UserID, a.B)
		return struct{}{}, err
	})
	// user.add inserts a row and deletes the bucket's oldest, so results keep
	// their size.
	cometgo.Method(app, "user.add", func(c *cometgo.Call, a bucketArgs) (struct{}, error) {
		if _, err := c.Tx.Exec(c.Context, `INSERT INTO cometload.user_items (owner, bucket, title) VALUES ($1, $2, 'added')`,
			c.Identity.UserID, a.B); err != nil {
			return struct{}{}, err
		}
		_, err := c.Tx.Exec(c.Context, `
			DELETE FROM cometload.user_items WHERE id = (
			  SELECT min(id) FROM cometload.user_items WHERE owner = $1 AND bucket = $2)`,
			c.Identity.UserID, a.B)
		return struct{}{}, err
	})
	cometgo.Method(app, "slow.fn", func(c *cometgo.Call, _ struct{}) (struct{}, error) {
		if _, err := c.Tx.Exec(c.Context, `SELECT pg_sleep($1::float8)`, p.slowFn.Seconds()); err != nil {
			return struct{}{}, err
		}
		_, err := c.Tx.Exec(c.Context, `UPDATE cometload.shared_items SET n = n + 1 WHERE id = 1`)
		return struct{}{}, err
	})

	if err := app.Start(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/_comet", app)
	mux.Handle("/stats", stats)
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		app.Shutdown(sctx)
		srv.Shutdown(sctx)
	}()
	fmt.Printf("addr %s\n", ln.Addr())
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}
