import { expect, test } from "bun:test";
import { dispatchToolSpecs } from "@legion/contracts";
import {
  type DispatchCommand,
  type DispatchToolName,
  dispatchCommands,
} from "../dispatch-commands/table";

// The whole point of typing the table `Record<DispatchToolName, DispatchCommand>`: a command the
// contract names and the table does not hold is refused by the type checker, never found at run
// time by the host that called it. The deletion that proves it cannot live in the tree, so the
// table is spread here with one command taken out of it and the hole handed to the same type.
// `bun run typecheck` fails on this file the day that stops being an error.
const { dispatch_read: _read, ...withoutRead } = dispatchCommands;

// @ts-expect-error - a table that does not hold every command the contract names is refused.
const missingOneCommand: Record<DispatchToolName, DispatchCommand> = withoutRead;

test("the table holds one handler for every command the contract names", () => {
  expect(Object.keys(dispatchCommands).sort()).toEqual(
    [...dispatchToolSpecs.map((spec) => spec.name)].sort()
  );
});

test("a table missing one command holds one fewer handler, and no type", () => {
  expect(Object.keys(missingOneCommand)).toHaveLength(dispatchToolSpecs.length - 1);
});
