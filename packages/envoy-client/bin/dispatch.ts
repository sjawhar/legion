// The `dispatch` command each host plugin bundles into its dist/dispatch.js and puts on its
// agents' shell PATH through a `bin/dispatch` shim. `runDispatchCli` holds the behaviour.
import { readFileSync } from "node:fs";
import { runDispatchCli } from "../src/dispatch-cli";

const readText = (path: string): string => readFileSync(path === "-" ? 0 : path, "utf-8");

process.exit(
  await runDispatchCli(Bun.argv.slice(2), process.env, {
    stdout: (text) => process.stdout.write(text),
    readText,
    cwd: process.cwd(),
  })
);
