// The `dispatch` command each host plugin bundles into its dist/dispatch.js and puts on its
// agents' shell PATH through a `bin/dispatch` shim. `runDispatchCli` holds the behaviour.
import { readFileSync } from "node:fs";
import { runDispatchCli } from "../src/dispatch-cli";

const readText = (path: string): string => readFileSync(path === "-" ? 0 : path, "utf-8");

// Bun's `process.exit` drops what a pipe has not yet taken (every omp session reads this command
// through one), so each write's callback is awaited before the process exits. A write that fails
// (the reader closed its end) is not retried: the reader no longer wants the rest.
const pending: Promise<void>[] = [];
const writeTo =
  (stream: NodeJS.WriteStream) =>
  (text: string): void => {
    const { promise, resolve } = Promise.withResolvers<void>();
    stream.write(text, () => resolve());
    pending.push(promise);
  };

const code = await runDispatchCli(Bun.argv.slice(2), process.env, {
  stdout: writeTo(process.stdout),
  stderr: writeTo(process.stderr),
  readText,
  cwd: process.cwd(),
});
await Promise.all(pending);
process.exit(code);
