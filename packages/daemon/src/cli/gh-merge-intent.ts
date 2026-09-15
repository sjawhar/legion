/**
 * Collects GraphQL query bodies that are literal argv values. `undefined` means the command
 * carries a file or stdin reference, so its operation cannot be classified safely.
 */
export function collectInlineGraphQLBodies(args: string[]): string[] | undefined {
  const bodies: string[] = [];
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index] ?? "";
    let field: string | undefined;
    if (arg === "-f" || arg === "-F" || arg === "--raw-field" || arg === "--field") {
      field = args[index + 1];
      index += 1;
    } else if (arg.startsWith("-f") || arg.startsWith("-F")) {
      field = arg.slice(2).replace(/^=/, "");
    } else if (arg.startsWith("--raw-field=")) {
      field = arg.slice("--raw-field=".length);
    } else if (arg.startsWith("--field=")) {
      field = arg.slice("--field=".length);
    }
    if (field !== undefined) {
      const separator = field.indexOf("=");
      if (field.slice(0, separator) === "query") {
        const query = field.slice(separator + 1);
        if (query.startsWith("@")) return undefined;
        bodies.push(query);
      }
      continue;
    }
    if (arg === "--input" || arg.startsWith("--input=")) return undefined;
  }
  return bodies;
}

export function bodyDeclaresGraphQLMutation(body: string): boolean {
  return /\bmutation\b/i.test(body);
}

function normalizedRestPath(token: string): string {
  return token.replace(/[?#].*$/, "").replace(/\/+$/, "");
}

/** Returns whether a `gh` invocation needs the controller-only merge guardrail. */
export function isGhMergeIntent(args: string[]): boolean {
  const positional = args.filter((arg) => !arg.startsWith("-"));
  const prIndex = positional.indexOf("pr");
  if (prIndex !== -1 && positional.slice(prIndex + 1).includes("merge")) return true;
  if (positional.includes("alias") || positional.includes("extension")) return true;
  if (!positional.includes("api")) return false;
  if (
    positional.some((token) => {
      const path = normalizedRestPath(token);
      return path.endsWith("/merge") || path.endsWith("/merges");
    })
  ) {
    return true;
  }
  if (
    !positional.some(
      (token) =>
        normalizedRestPath(token) === "graphql" || normalizedRestPath(token).endsWith("/graphql")
    )
  ) {
    return false;
  }
  const bodies = collectInlineGraphQLBodies(args);
  return bodies === undefined || bodies.some(bodyDeclaresGraphQLMutation);
}
