import { CometError, connect } from "../../../../client/src/index.ts";

interface Todo {
  id: string;
  title: string;
  done: boolean;
  created_at: Date;
}

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const list = $<HTMLUListElement>("list");
const status = $("status");
const count = $("count");
const timing = $("timing");
const errorBox = $("error");
const titleInput = $<HTMLInputElement>("title");

const comet = connect();
const todos = comet.subscribe<Todo>("todos.all");

comet.onChange(() => {
  status.textContent = comet.status;
  status.className = `pill ${comet.status}`;
});

todos.subscribe(render);
render();

function render(): void {
  list.classList.toggle("stale", todos.status === "resyncing");
  list.replaceChildren(
    ...todos.rows.map((t) => {
      const li = document.createElement("li");
      li.className = t.done ? "done" : "";
      const box = document.createElement("input");
      box.type = "checkbox";
      box.checked = t.done;
      box.onchange = () => run("todos.toggle", { id: t.id });
      const title = document.createElement("span");
      title.className = "title";
      title.textContent = t.title;
      title.title = `added ${t.created_at.toLocaleString()}`;
      const del = document.createElement("button");
      del.className = "link";
      del.textContent = "✕";
      del.setAttribute("aria-label", "Delete");
      del.onclick = () => run("todos.remove", { id: t.id });
      li.append(box, title, del);
      return li;
    }),
  );
  if (todos.status === "loading") list.innerHTML = `<li class="empty">Loading…</li>`;
  else if (todos.rows.length === 0) list.innerHTML = `<li class="empty">Nothing to do.</li>`;
  const left = todos.rows.filter((t) => !t.done).length;
  count.textContent = `${left} left`;
}

async function run(method: string, args: unknown): Promise<void> {
  errorBox.hidden = true;
  const start = performance.now();
  try {
    const { lag } = await comet.callWithStatus(method, args);
    // The call resolves only after its effects reached our subscription,
    // so the list above is already up to date here.
    timing.textContent = `${method} ${Math.round(performance.now() - start)} ms${lag ? " (lagging)" : ""}`;
  } catch (e) {
    errorBox.hidden = false;
    errorBox.textContent = e instanceof CometError ? `${e.code}: ${e.message}` : String(e);
  }
}

$<HTMLFormElement>("add").onsubmit = async (ev) => {
  ev.preventDefault();
  const title = titleInput.value;
  titleInput.value = "";
  await run("todos.add", { title });
  titleInput.focus();
};

$("clear").onclick = () => run("todos.clearDone", null);
