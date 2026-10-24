// AskGate's prompts and rules are markdown files that Bun imports as text
// (`import rules from "./rules.md" with { type: "text" }`). Bun's own types declare `*.txt` and not
// `*.md`, so this declares the markdown module for the type checker.
declare module "*.md" {
  const text: string;
  export default text;
}
