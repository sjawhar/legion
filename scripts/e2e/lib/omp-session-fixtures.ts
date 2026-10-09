// An Oh My Pi session transcript's entries, built as Oh My Pi writes them, for the tests of the live
// proofs' readers that omp-tool-calls.jq serves. Oh My Pi gives the model four ways to call a tool,
// and a reader must count each: the tool itself, a write to its xd://<name> device whose content is
// the arguments' JSON, eval code that calls tool.<name>(...), and eval code that calls the generic
// tool.write(...) naming that device.

export type Entry = Record<string, unknown>;
export type Call = Record<string, unknown>;

let ids = 0;

/** A call of the tool NAME with ARGS, the tool itself. */
export const toolCall = (name: string, args: Record<string, unknown>): Call => ({
  type: "toolCall",
  id: `toolu_${++ids}`,
  name,
  arguments: args,
});

/** A call of the tool NAME through a write to its xd://NAME device. */
export const deviceCall = (name: string, args: Record<string, unknown>): Call =>
  toolCall("write", { path: `xd://${name}`, content: JSON.stringify(args), i: "Post" });

/** Eval code CODE, which calls tools as tool.<name>(...). */
export const evalCall = (code: string): Call =>
  toolCall("eval", { language: "js", title: "post", code });

/** The four surfaces of one bash call running COMMAND. In both eval forms the command is a string
 * literal of the code, escaped once, as the model writes it. */
export const bashTool = (command: string): Call => toolCall("bash", { command });
export const bashDevice = (command: string): Call => deviceCall("bash", { command });
export const bashEval = (command: string): Call =>
  evalCall(`await tool.bash({ command: ${JSON.stringify(command)} });`);
export const bashEvalWrite = (command: string): Call =>
  evalCall(
    `await tool.write({ path: "xd://bash", content: JSON.stringify({ command: ${JSON.stringify(command)} }), i: "Run" });`
  );
export const bashSurfaces = {
  tool: bashTool,
  device: bashDevice,
  eval: bashEval,
  evalWrite: bashEvalWrite,
} as const;

const turn = (stopReason: string, calls: Call[]): Entry => ({
  type: "message",
  message: { role: "assistant", stopReason, content: [{ type: "text", text: "…" }, ...calls] },
});

/** An assistant message that makes CALLS, which Oh My Pi ends with stopReason `toolUse`. */
export const assistant = (...calls: Call[]): Entry => turn("toolUse", calls);

/** An assistant message that makes no call and ends the turn: `stop`, or an `error` not retried. */
export const turnEnd = (stopReason: "stop" | "error"): Entry => turn(stopReason, []);

/** A tool result whose text is TEXT. */
export const toolResult = (text: string): Entry => ({
  type: "message",
  message: { role: "toolResult", content: [{ type: "text", text }] },
});

/** A session file's contents: one JSON entry per line. */
export const jsonl = (entries: readonly unknown[]): string =>
  `${entries.map((entry) => JSON.stringify(entry)).join("\n")}\n`;
