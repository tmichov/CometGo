// Command todo is a CometGo demo: a shared todo list that stays live across
// every open tab. The Go binary serves both the UI and the realtime endpoint.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmichov/cometgo"
)

//go:embed all:web/public
var webFS embed.FS

const schema = `
CREATE TABLE IF NOT EXISTS todos (
  id         bigserial   PRIMARY KEY,
  title      text        NOT NULL,
  done       boolean     NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
)`

type addArgs struct {
	Title string `json:"title"`
}

type idArgs struct {
	ID string `json:"id"` // int8 travels as a decimal string
}

type added struct {
	ID string `json:"id"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dsn := env("DATABASE_URL", "postgres://comet:comet@localhost:54329/comet")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, schema); err != nil {
		log.Fatal(err)
	}
	if err := cometgo.InstallTriggers(ctx, pool, "todos"); err != nil {
		log.Fatal(err)
	}

	app := cometgo.New(cometgo.Config{DB: pool})

	cometgo.Publish(app, "todos.all", cometgo.PubOptions{Tables: []string{"todos"}, Scope: cometgo.ScopeShared},
		func(c *cometgo.SubCtx, _ struct{}) (cometgo.Query, error) {
			return cometgo.Query{SQL: `SELECT id, title, done, created_at FROM todos ORDER BY created_at, id`}, nil
		})

	cometgo.Method(app, "todos.add", func(c *cometgo.Call, a addArgs) (added, error) {
		title := strings.TrimSpace(a.Title)
		if title == "" {
			return added{}, cometgo.ValidationError(map[string]string{"title": "required"})
		}
		var out added
		err := c.Tx.QueryRow(c.Context, `INSERT INTO todos (title) VALUES ($1) RETURNING id::text`, title).Scan(&out.ID)
		return out, err
	})

	cometgo.Method(app, "todos.toggle", func(c *cometgo.Call, a idArgs) (struct{}, error) {
		tag, err := c.Tx.Exec(c.Context, `UPDATE todos SET done = NOT done WHERE id = $1::int8`, a.ID)
		if err == nil && tag.RowsAffected() == 0 {
			err = cometgo.NewError(cometgo.CodeNotFound, "no such todo")
		}
		return struct{}{}, err
	})

	cometgo.Method(app, "todos.remove", func(c *cometgo.Call, a idArgs) (struct{}, error) {
		_, err := c.Tx.Exec(c.Context, `DELETE FROM todos WHERE id = $1::int8`, a.ID)
		return struct{}{}, err
	})

	cometgo.Method(app, "todos.clearDone", func(c *cometgo.Call, _ struct{}) (int64, error) {
		tag, err := c.Tx.Exec(c.Context, `DELETE FROM todos WHERE done`)
		return tag.RowsAffected(), err
	})

	app.Start(ctx)

	public, err := fs.Sub(webFS, "web/public")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/_comet", app)
	mux.Handle("/", http.FileServerFS(public))

	addr := env("ADDR", ":8080")
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("todo: listening on http://localhost%s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
