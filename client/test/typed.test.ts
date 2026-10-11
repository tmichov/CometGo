// Compile-time checks of connect<Schema>() and hooksFor<Schema>(), against
// the todo example's generated schema. `npm run typecheck` fails if a line
// marked as an expected error stops being one. Nothing here runs a call.

import { test } from "node:test";
import type { Schema } from "../../examples/todo/web/src/schema.ts";
import type { TypedComet } from "../src/index.ts";
import type { TypedHooks } from "../src/react.ts";

function checks(comet: TypedComet<Schema>, hooks: TypedHooks<Schema>): void {
  const added: Promise<{ id: string }> = comet.call("todos.add", { title: "x" });
  const cleared: Promise<{ value: number; lag: boolean }> = comet.callWithStatus("todos.clearDone");
  comet.call("todos.toggle", { id: "1" }, { noFence: true });
  const rows: readonly { id: string; title: string; done: boolean; created_at: Date }[] = comet.subscribe("todos.all").rows;
  void [added, cleared, rows];

  // @ts-expect-error unknown method
  comet.call("todos.ad", { title: "x" });
  // @ts-expect-error wrong arg field
  comet.call("todos.add", { name: "x" });
  // @ts-expect-error wrong arg type
  comet.call("todos.toggle", { id: 1 });
  // @ts-expect-error missing args
  comet.call("todos.add");
  // @ts-expect-error wrong result type
  const bad: Promise<string> = comet.call("todos.add", { title: "x" });
  // @ts-expect-error unknown publication
  comet.subscribe("todos.everything");
  // @ts-expect-error no such column
  comet.subscribe("todos.all").rows[0].missing;
  void bad;

  const call = hooks.useCall();
  call("todos.remove", { id: "1" });
  // @ts-expect-error unknown method through useCall
  call("todos.delete", { id: "1" });
  const done: boolean = hooks.useSubscription("todos.all").rows[0].done;
  // @ts-expect-error unknown publication through useSubscription
  hooks.useSubscription("nope");
  void done;
}

test("typed client compiles", () => {
  void checks;
});
