// tsc rewrites relative ".ts" imports to ".js" in the emitted JS but leaves
// them as ".ts" in the .d.ts files, which TypeScript before 5.0 cannot
// resolve. Rewrite them the same way. dist/ is flat. Side-effect imports
// (react-native-get-random-values) carry no types and fail a consumer's
// check under noUncheckedSideEffectImports, so they are dropped.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";

for (const f of readdirSync("dist")) {
  if (!f.endsWith(".d.ts")) continue;
  const p = `dist/${f}`;
  writeFileSync(
    p,
    readFileSync(p, "utf8")
      .replace(/(["']\.\.?\/[^"']+)\.ts(["'])/g, "$1.js$2")
      .replace(/^import ["'][^"']+["'];\n/gm, ""),
  );
}
