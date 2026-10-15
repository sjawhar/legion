import { type EnvoyConfigEnvironment, fileValue, httpUrl, nonEmpty } from "./envoy-config";
import { readSecretFile, sendable } from "./secret-file";

/** A bearer written in an envoy.json, and the server written beside it: the only server it may go
 * to. */
export interface FileBearer {
  readonly token: string;
  /** Where it is written: `dispatch.token` or `envoy.token`. */
  readonly field: string;
  /** The file it is written in. */
  readonly path: string;
  /** The server written beside it, or the section's default address when the file names none. */
  readonly server: string;
  /** A checkout's file, which the repository controls: a `!` value there is refused, never run. */
  readonly checkout: boolean;
}

/** Where a section's bearer comes from: a variable, which goes to whatever server resolves; a
 * file, whose bearer goes only to its own server; or nowhere. */
export type BearerSource =
  | { readonly kind: "variable"; readonly token: string }
  | ({ readonly kind: "file" } & FileBearer)
  | { readonly kind: "none" };

/** The section a bearer is for, which names its variables: `<SECTION>_URL`, `<SECTION>_TOKEN`,
 * `<SECTION>_TOKEN_FILE`. */
export type BearerSection = "DISPATCH" | "ENVOY";

/**
 * Where `section`'s bearer comes from: `<SECTION>_TOKEN_FILE` (the trimmed contents of the file it
 * names, how the Legion daemon delivers it to a pane), then `<SECTION>_TOKEN`, then the bearer
 * `written` gives, which is asked for only when neither variable is set. An empty variable counts
 * as unset. Throws naming the variable whose file cannot be read or whose value is not `sendable`,
 * never falling back to the next source, and whatever `written` throws.
 */
export function bearerSource(
  env: EnvoyConfigEnvironment,
  section: BearerSection,
  written: () => FileBearer | undefined
): BearerSource {
  const pointer = nonEmpty(env[`${section}_TOKEN_FILE`]);
  if (pointer !== undefined) {
    return { kind: "variable", token: readSecretFile(`${section}_TOKEN_FILE`, pointer) };
  }
  const plain = nonEmpty(env[`${section}_TOKEN`]);
  if (plain !== undefined) {
    if (!sendable(plain)) {
      throw new Error(`${section}_TOKEN holds whitespace or a control character`);
    }
    return { kind: "variable", token: plain };
  }
  const bearer = written();
  return bearer === undefined ? { kind: "none" } : { kind: "file", ...bearer };
}

/**
 * The bearer `bearer`'s file gives for `url`. Same server only: it goes to `url` only when that
 * names the origin of the server written beside it, which only `<SECTION>_URL` can make differ, and
 * is otherwise refused naming the variables that give another server its bearer. Then a checkout's
 * `!` value is refused, an empty value is refused, and the value is read (`fileValue`, which runs a
 * chosen file's `!` command and refuses whitespace inside a value). Throws naming the field and the
 * file, never the value.
 */
export function fileBearer(
  bearer: FileBearer,
  url: string,
  section: BearerSection,
  env: EnvoyConfigEnvironment
): string {
  const named = httpUrl(url)?.origin;
  const paired = httpUrl(bearer.server)?.origin;
  if (named !== paired) {
    throw new Error(
      `${section}_URL names ${named}, but ${bearer.path}'s ${bearer.field} is for ${paired}; set ${section}_TOKEN or ${section}_TOKEN_FILE for it`
    );
  }
  if (bearer.checkout && bearer.token.startsWith("!")) {
    throw new Error(
      `${bearer.path}: ${bearer.field} is a command, which a checkout's envoy.json cannot run`
    );
  }
  if (bearer.token === "") {
    throw new Error(`${bearer.field} in ${bearer.path} must be a non-empty bearer token`);
  }
  return fileValue(bearer.token, bearer.field, bearer.path, env);
}
