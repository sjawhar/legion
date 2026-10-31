// Markdown files that Bun imports as text (`import rules from "./rules.md" with { type: "text" }`),
// as AskGate's prompts and rules in pi-envoy are. Bun's own types declare `*.txt` and not `*.md`,
// so this declares the markdown module for the type checker. Each package that type-checks such an
// import includes this file in its tsconfig, as it includes omp-host.d.ts.
declare module "*.md" {
  const text: string;
  export default text;
}
