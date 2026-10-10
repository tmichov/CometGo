// The todo demo written with the React bindings. Same server, same UI as
// main.ts; served at /react.html.

import { type FormEvent, StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { CometError, connect } from "../../../../client/src/index.ts";
import { CometProvider, useCall, useConnection, useSubscription } from "../../../../client/src/react.ts";

interface Todo {
  id: string;
  title: string;
  done: boolean;
  created_at: Date;
}

function App() {
  const { status } = useConnection();
  const todos = useSubscription<Todo>("todos.all");
  const call = useCall();
  const [title, setTitle] = useState("");
  const [error, setError] = useState<string | null>(null);

  async function run(method: string, args: unknown): Promise<void> {
    setError(null);
    try {
      // Resolves only after its effects reached our subscription, so the
      // list already shows the change when this returns.
      await call(method, args);
    } catch (e) {
      setError(e instanceof CometError ? `${e.code}: ${e.message}` : String(e));
    }
  }

  async function add(ev: FormEvent): Promise<void> {
    ev.preventDefault();
    const t = title;
    setTitle("");
    await run("todos.add", { title: t });
  }

  const left = todos.rows.filter((t) => !t.done).length;
  return (
    <main>
      <header>
        <h1>Todos (React)</h1>
        <span className={`pill ${status}`}>{status}</span>
      </header>
      <div className="card">
        <form onSubmit={add}>
          <input type="text" placeholder="What needs doing?" autoComplete="off" value={title} onChange={(e) => setTitle(e.target.value)} />
          <button type="submit">Add</button>
        </form>
        {error && <div className="error">{error}</div>}
        <ul className={todos.status === "resyncing" ? "stale" : ""}>
          {todos.status === "loading" ? (
            <li className="empty">Loading…</li>
          ) : todos.rows.length === 0 ? (
            <li className="empty">Nothing to do.</li>
          ) : (
            todos.rows.map((t) => (
              <li key={t.id} className={t.done ? "done" : ""}>
                <input type="checkbox" checked={t.done} onChange={() => run("todos.toggle", { id: t.id })} />
                <span className="title" title={`added ${t.created_at.toLocaleString()}`}>
                  {t.title}
                </span>
                <button className="link" aria-label="Delete" onClick={() => run("todos.remove", { id: t.id })}>
                  ✕
                </button>
              </li>
            ))
          )}
        </ul>
        <footer>
          <span>{left} left</span>
          <span />
          <button className="link" type="button" onClick={() => run("todos.clearDone", null)}>
            Clear done
          </button>
        </footer>
      </div>
      <p className="hint">
        Open this page next to <a href="/">the plain TypeScript version</a>: both stay in sync.
      </p>
    </main>
  );
}

const comet = connect();
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <CometProvider client={comet}>
      <App />
    </CometProvider>
  </StrictMode>,
);
