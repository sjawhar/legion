const NON_INTERACTIVE_ENV: Record<string, string> = {
  GIT_TERMINAL_PROMPT: "0",
  GIT_EDITOR: ":",
  EDITOR: ":",
  VISUAL: "",
  GIT_SEQUENCE_EDITOR: ":",
  GIT_MERGE_AUTOEDIT: "no",
  GIT_PAGER: "cat",
  PAGER: "cat",
  GCM_INTERACTIVE: "never",
};

export function nonInteractiveEnvHook(
  _input: { cwd: string },
  output: { env: Record<string, string> }
): void {
  for (const [key, value] of Object.entries(NON_INTERACTIVE_ENV)) {
    if (output.env[key] === undefined) {
      output.env[key] = value;
    }
  }
}
