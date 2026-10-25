import { parseArgs } from "node:util";
import { SQL } from "bun";
import { z } from "zod";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import {
  DISPATCH_TOOL_DEADLINE_MS,
  DispatchClient,
  DispatchServiceError,
} from "../../packages/envoy-client/src/dispatch-http";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";

/** A production check of Dispatch meaning search (INFRA-194). Everything it prints is from an
 * allowlist: validated keys, kind names, positions, counts, the validated build commit, the
 * place, match or mismatch, and a fixed failure category. It never prints a URL, the token, a
 * response body, an issue title, a query, the coverage identity or an exception's text, since
 * the repository is public and those carry private hosts and names. */

const PAIR_KEY = /^[A-Z][A-Z0-9]*-[1-9][0-9]*$/;
const WHOLE_SECONDS = /^[0-9]+$/;
const POSTGRES_URL = /^postgres(?:ql)?:\/\//;
/** A2 looks for the issue's own row among this many results, and asks search for no more. */
const A2_DEPTH = 5;
const COVERAGE_INTERVAL_MS = 5000;
const DEFAULT_COVERAGE_WAIT_SECONDS = 600;
/** `search_text`'s pattern and replacement (`migrations/0056_search_text.up.sql`), bound as
 * parameters rather than created as a temporary function. */
const SEARCH_TEXT_PATTERN = "((?:[A-Za-z0-9.-]+_){16})";
const SEARCH_TEXT_REPLACEMENT = "\\1 ";

const USAGE = {
  arguments: "unknown option, missing option value or stray argument",
  check: "give at least one of --a1-query, --pair, --coverage",
  coverage: "--coverage and --expect-model go together",
  wait: "--coverage-wait goes with --coverage and takes a whole number of seconds",
  pair: "--pair takes KEY=QUERY: a key such as ABC-12, then a query",
  a1: "--a1-query takes a query",
  lexer: "--lexeme-database takes a postgres URL or the path of a .s.PGSQL.5432 socket",
} as const;

/** `GET /healthz`, as Dispatch answers it on 200 and on 503. */
const Health = z.looseObject({
  ok: z.boolean(),
  commit: z
    .string()
    .regex(/^[0-9a-f]{7,40}$/)
    .nullable(),
  schema_version: z.int().min(0).nullable(),
});
export type Health = z.infer<typeof Health>;

const SearchAnswer = z.looseObject({ results: z.array(z.unknown()) });

/** The fields of a search result A2 reads. */
const SearchHit = z.looseObject({
  kind: z.string(),
  owner: z.looseObject({ kind: z.string(), key: z.unknown() }),
});

const IssueIdentity = z.looseObject({ key: z.string(), title: z.string() });

const Count = z.int().min(0);

const CoverageCounts = z
  .looseObject({
    items: Count,
    current: Count,
    lacking: Count,
    failed: Count,
    oldest_lacking_seconds: z.number().min(0).nullable(),
  })
  .refine((counts) => counts.items === counts.current + counts.lacking + counts.failed);
export type CoverageCounts = z.infer<typeof CoverageCounts>;

/** INFRA-393's coverage report, `GET /api/v1/search/coverage`. Its kinds are whatever it lists. */
const CoverageReport = z.looseObject({
  identity: z.string().nullable(),
  place: z.enum(["bedrock", "azure"]).nullable(),
  total: CoverageCounts,
  kinds: z
    .record(z.string().regex(/^[a-z][a-z0-9_]{0,31}$/), CoverageCounts)
    .refine((kinds) => Object.keys(kinds).length > 0),
});
export type CoverageReport = z.infer<typeof CoverageReport>;

/** One row of the lexer's statement, as Bun's `SQL` answers it. */
const LexemeRows = z.tuple([z.looseObject({ issue: z.array(z.string()), query: z.string() })]);

export type Verdict = "pass" | "fail" | "unavailable";

export type Category =
  | "auth"
  | "transport"
  | "malformed"
  | "not-found"
  | "lexeme-database"
  | "config";

export interface Pair {
  readonly key: string;
  readonly query: string;
}

export interface Options {
  readonly a1Query?: string;
  readonly pairs: readonly Pair[];
  readonly lexemeDatabase?: string;
  readonly coverage?: { readonly expectModel: string; readonly waitSeconds: number };
}

/** The flags as `parseArgs` reads them, before their rules are checked. */
interface Flags {
  readonly "a1-query"?: string;
  readonly pair?: string[];
  readonly "lexeme-database"?: string;
  readonly coverage?: boolean;
  readonly "expect-model"?: string;
  readonly "coverage-wait"?: string;
}

/** A command line the script refuses. Its message is fixed text that never repeats an argument. */
export class UsageError extends Error {
  override readonly name = "UsageError";
}

/** A status the script's own reads do not accept. */
export class HttpStatusError extends Error {
  override readonly name = "HttpStatusError";

  constructor(readonly status: number) {
    super(`HTTP ${status}`);
  }
}

/** A body that is not JSON at all. A JSON body of the wrong shape fails its schema instead. */
export class MalformedAnswer extends Error {
  override readonly name = "MalformedAnswer";
}

/** The reads the checks make of Dispatch, each answering the parsed JSON it received. */
export interface DispatchReads {
  /** `GET /healthz`, whose body is read on 200 and on 503. */
  healthz(): Promise<unknown>;
  search(query: string, options?: { readonly limit: number }): Promise<unknown>;
  issue(key: string): Promise<unknown>;
  /** `GET /api/v1/search/coverage`. */
  coverage(): Promise<unknown>;
}

/** Postgres's `english` lexemes for an issue's own search row and for a query. */
export interface Lexemes {
  readonly issue: readonly string[];
  readonly query: readonly string[];
}

export interface Lexer {
  lexemes(key: string, title: string, query: string): Promise<Lexemes>;
}

export interface Clock {
  now(): number;
  sleep(ms: number): Promise<void>;
}

export interface CheckDependencies {
  readonly dispatch: DispatchReads;
  readonly lexer?: Lexer;
  readonly clock?: Clock;
  readonly out: (line: string) => void;
  readonly err: (line: string) => void;
}

export interface A1Judgement {
  readonly degraded: boolean;
  readonly results: number;
  readonly verdict: "pass" | "fail";
}

export interface PairResult {
  readonly key: string;
  readonly verdict: Verdict;
  /** What the pair's search found, or null when the search itself was unavailable. */
  readonly found: { readonly shared: number | null; readonly position: number | null } | null;
  readonly category: Category | null;
}

export interface CoverageJudgement {
  readonly verdict: "pass" | "fail";
  readonly identityMatch: boolean;
  /** Whether lacking items are all that keeps the snapshot from passing, so a later read may. */
  readonly onlyLacking: boolean;
}

export type CoverageResult =
  | { readonly verdict: "unavailable"; readonly category: Category; readonly reads: number }
  | {
      readonly verdict: "pass" | "fail";
      readonly report: CoverageReport;
      readonly identityMatch: boolean;
      readonly reads: number;
    };

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "token">;

function parsePair(value: string): Pair {
  const split = value.indexOf("=");
  if (split < 0) throw new UsageError(USAGE.pair);
  const key = value.slice(0, split);
  const query = value.slice(split + 1);
  if (!PAIR_KEY.test(key) || query.trim() === "") throw new UsageError(USAGE.pair);
  return { key, query };
}

export function parseArguments(argv: readonly string[]): Options {
  let flags: Flags;
  try {
    flags = parseArgs({
      args: [...argv],
      options: {
        "a1-query": { type: "string" },
        pair: { type: "string", multiple: true },
        "lexeme-database": { type: "string" },
        coverage: { type: "boolean" },
        "expect-model": { type: "string" },
        "coverage-wait": { type: "string" },
      },
      strict: true,
    }).values;
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE.arguments);
  }
  const a1Query = flags["a1-query"];
  const lexemeDatabase = flags["lexeme-database"];
  const expectModel = flags["expect-model"];
  const wait = flags["coverage-wait"];
  const coverage = flags.coverage === true;
  const pairs = (flags.pair ?? []).map(parsePair);
  if (a1Query === undefined && pairs.length === 0 && !coverage) throw new UsageError(USAGE.check);
  if (coverage !== (expectModel !== undefined)) throw new UsageError(USAGE.coverage);
  if (wait !== undefined && (!coverage || !WHOLE_SECONDS.test(wait))) {
    throw new UsageError(USAGE.wait);
  }
  const waitSeconds = wait === undefined ? DEFAULT_COVERAGE_WAIT_SECONDS : Number(wait);
  if (!Number.isSafeInteger(waitSeconds)) throw new UsageError(USAGE.wait);
  if (a1Query !== undefined && a1Query.trim() === "") throw new UsageError(USAGE.a1);
  if (lexemeDatabase === "") throw new UsageError(USAGE.lexer);
  return {
    pairs,
    ...(a1Query === undefined ? {} : { a1Query }),
    ...(lexemeDatabase === undefined ? {} : { lexemeDatabase }),
    ...(expectModel === undefined ? {} : { coverage: { expectModel, waitSeconds } }),
  };
}

/** The fixed category an error is reported as; its own text never is. */
export function errorCategory(error: unknown): Category {
  if (error instanceof MalformedAnswer || error instanceof z.ZodError) return "malformed";
  if (!(error instanceof DispatchServiceError || error instanceof HttpStatusError)) {
    return "transport";
  }
  if (error.status === 401 || error.status === 403) return "auth";
  if (error.status === 404) return "not-found";
  return "transport";
}

/** Any unavailable check → 2; else any failed check → 1; else 0. */
export function exitCode(verdicts: readonly Verdict[]): 0 | 1 | 2 {
  if (verdicts.includes("unavailable")) return 2;
  if (verdicts.includes("fail")) return 1;
  return 0;
}

/**
 * Each lexeme `querytree` quotes, once each, in order. Inside a quoted lexeme Postgres doubles a
 * quote and a backslash. `querytree` answers `T` (or nothing) when no part of the query is
 * indexable, which holds no lexeme.
 */
export function queryTreeLexemes(text: string): string[] {
  const lexemes = new Set<string>();
  for (const match of text.matchAll(/'((?:[^'\\]|''|\\.)*)'/gs)) {
    const quoted = match[1] ?? "";
    lexemes.add(quoted.replace(/''|\\(.)/gs, (_escape, escaped?: string) => escaped ?? "'"));
  }
  return [...lexemes];
}

/** How many distinct query lexemes the issue's own row also holds. */
export function sharedLexemes(lexemes: Lexemes): number {
  const issue = new Set(lexemes.issue);
  return new Set(lexemes.query.filter((lexeme) => issue.has(lexeme))).size;
}

/** A1 passes when search answered without `degraded`, so meaning search ran for the request. */
export function judgeA1(response: unknown): A1Judgement {
  const answer = SearchAnswer.parse(response);
  const degraded = Object.hasOwn(answer, "degraded");
  return { degraded, results: answer.results.length, verdict: degraded ? "fail" : "pass" };
}

/**
 * The 1-based position, within the first `A2_DEPTH` results, of the issue's own row: kind
 * `issue`, owned by the issue. A document, comment, ask or message the issue owns never counts.
 */
export function searchPosition(response: unknown, key: string): number | null {
  const hits = z.array(SearchHit).parse(SearchAnswer.parse(response).results.slice(0, A2_DEPTH));
  const index = hits.findIndex(
    (hit) => hit.kind === "issue" && hit.owner.kind === "issue" && hit.owner.key === key
  );
  return index < 0 ? null : index + 1;
}

/** A pair passes when its query shares no lexeme with the issue's own row and finds that row. */
export function judgeA2Pair(shared: number, position: number | null): "pass" | "fail" {
  return shared === 0 && position !== null ? "pass" : "fail";
}

/**
 * One pair: the lexemes its query shares with the issue's own row (when a lexer is given), and
 * the issue row's position in the query's top five. A failed search or issue read makes the pair
 * unavailable with its category; otherwise a missing or failed lexer does (`lexeme-database`).
 */
export async function checkPair(
  pair: Pair,
  dispatch: DispatchReads,
  lexer: Lexer | undefined
): Promise<PairResult> {
  let shared: number | null = null;
  let issueFailure: Category | null = null;
  if (lexer !== undefined) {
    let issue: z.infer<typeof IssueIdentity> | null = null;
    try {
      issue = IssueIdentity.parse(await dispatch.issue(pair.key));
    } catch (error) {
      issueFailure = errorCategory(error);
    }
    if (issue !== null) {
      try {
        shared = sharedLexemes(await lexer.lexemes(issue.key, issue.title, pair.query));
      } catch {
        // Reported as lexeme-database below.
      }
    }
  }
  let position: number | null;
  try {
    position = searchPosition(await dispatch.search(pair.query, { limit: A2_DEPTH }), pair.key);
  } catch (error) {
    return { key: pair.key, verdict: "unavailable", found: null, category: errorCategory(error) };
  }
  const found = { shared, position };
  if (issueFailure !== null) {
    return { key: pair.key, verdict: "unavailable", found, category: issueFailure };
  }
  if (shared === null) {
    return { key: pair.key, verdict: "unavailable", found, category: "lexeme-database" };
  }
  return { key: pair.key, verdict: judgeA2Pair(shared, position), found, category: null };
}

/** A2 passes when any pair passes; else it is unavailable when any pair is; else it fails. */
export function judgeA2(pairs: readonly Pick<PairResult, "verdict">[]): Verdict {
  if (pairs.some((pair) => pair.verdict === "pass")) return "pass";
  if (pairs.some((pair) => pair.verdict === "unavailable")) return "unavailable";
  return "fail";
}

/** The coverage report, checked field by field; any departure from its shape is malformed. */
export function parseCoverage(body: unknown): CoverageReport {
  return CoverageReport.parse(body);
}

/** A snapshot passes when its identity is the expected model and every item is current. */
export function judgeCoverage(report: CoverageReport, expectModel: string): CoverageJudgement {
  const identityMatch = report.identity === expectModel;
  const { items, failed, lacking } = report.total;
  const otherwiseHolds = identityMatch && items > 0 && failed === 0;
  return {
    verdict: otherwiseHolds && lacking === 0 ? "pass" : "fail",
    identityMatch,
    onlyLacking: otherwiseHolds && lacking > 0,
  };
}

/**
 * Reads the coverage report until a snapshot passes. It reads again, `COVERAGE_INTERVAL_MS`
 * later, only while lacking items alone keep a snapshot from passing and the next read still
 * falls inside `waitSeconds`; otherwise the last snapshot fails. Any unavailable read ends it as
 * unavailable.
 */
export async function checkCoverage(
  readCoverage: () => Promise<unknown>,
  expectModel: string,
  waitSeconds: number,
  clock: Clock
): Promise<CoverageResult> {
  const start = clock.now();
  let reads = 0;
  for (;;) {
    reads += 1;
    let report: CoverageReport;
    try {
      report = parseCoverage(await readCoverage());
    } catch (error) {
      return { verdict: "unavailable", category: errorCategory(error), reads };
    }
    const judgement = judgeCoverage(report, expectModel);
    const readAgain =
      judgement.onlyLacking && clock.now() - start + COVERAGE_INTERVAL_MS <= waitSeconds * 1000;
    if (!readAgain) {
      return { verdict: judgement.verdict, report, identityMatch: judgement.identityMatch, reads };
    }
    await clock.sleep(COVERAGE_INTERVAL_MS);
  }
}

function countsLine(counts: CoverageCounts): string {
  const oldest = counts.oldest_lacking_seconds ?? "none";
  return (
    `items=${counts.items} current=${counts.current} lacking=${counts.lacking} ` +
    `oldest_lacking_s=${oldest} failed=${counts.failed}`
  );
}

function formatPair(pair: PairResult): string {
  if (pair.found === null) return `A2 ${pair.key} unavailable: ${pair.category}`;
  const shared = pair.found.shared ?? "unchecked";
  const position = pair.found.position ?? "none";
  const verdict = pair.verdict === "unavailable" ? `unavailable: ${pair.category}` : pair.verdict;
  return `A2 ${pair.key} shared=${shared} position=${position} ${verdict}`;
}

function formatCoverage(result: CoverageResult): string[] {
  if (result.verdict === "unavailable") {
    return [`A3 unavailable: ${result.category} reads=${result.reads}`];
  }
  const { report } = result;
  return [
    `A3 identity=${result.identityMatch ? "match" : "mismatch"} place=${report.place ?? "none"} ` +
      `${countsLine(report.total)} reads=${result.reads} ${result.verdict}`,
    ...Object.entries(report.kinds).map(
      ([name, counts]) => `A3 kind=${name} ${countsLine(counts)}`
    ),
  ];
}

async function checkBuild(dispatch: DispatchReads, out: (line: string) => void): Promise<Verdict> {
  let health: Health;
  try {
    health = Health.parse(await dispatch.healthz());
  } catch (error) {
    out(`build unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
  const schema = health.schema_version ?? "null";
  out(`build commit=${health.commit ?? "null"} schema=${schema} ok=${health.ok}`);
  return "pass";
}

async function checkA1(
  query: string,
  dispatch: DispatchReads,
  out: (line: string) => void
): Promise<Verdict> {
  let judgement: A1Judgement;
  try {
    judgement = judgeA1(await dispatch.search(query));
  } catch (error) {
    out(`A1 unavailable: ${errorCategory(error)}`);
    return "unavailable";
  }
  const degraded = judgement.degraded ? "yes" : "no";
  out(`A1 degraded=${degraded} results=${judgement.results} ${judgement.verdict}`);
  return judgement.verdict;
}

const realClock: Clock = { now: () => Date.now(), sleep: (ms) => Bun.sleep(ms) };

/** Runs the build read and each check the options ask for, prints their lines, and answers the
 * exit code. */
export async function runChecks(options: Options, deps: CheckDependencies): Promise<number> {
  const { dispatch, out } = deps;
  try {
    const verdicts: Verdict[] = [await checkBuild(dispatch, out)];
    if (options.a1Query !== undefined) verdicts.push(await checkA1(options.a1Query, dispatch, out));
    if (options.pairs.length > 0) {
      const pairs: PairResult[] = [];
      for (const pair of options.pairs) {
        const result = await checkPair(pair, dispatch, deps.lexer);
        out(formatPair(result));
        pairs.push(result);
      }
      const verdict = judgeA2(pairs);
      const passed = pairs.filter((pair) => pair.verdict === "pass").length;
      out(`A2 ${verdict} (pairs passed ${passed} of ${pairs.length})`);
      verdicts.push(verdict);
    }
    if (options.coverage !== undefined) {
      const result = await checkCoverage(
        () => dispatch.coverage(),
        options.coverage.expectModel,
        options.coverage.waitSeconds,
        deps.clock ?? realClock
      );
      for (const line of formatCoverage(result)) out(line);
      verdicts.push(result.verdict);
    }
    const code = exitCode(verdicts);
    const result = code === 2 ? "unavailable" : code === 1 ? "fail" : "pass";
    out(`result ${result} exit=${code}`);
    return code;
  } catch (error) {
    deps.err(`unavailable: ${errorCategory(error)}`);
    return 2;
  }
}

/** Parsed JSON from one of the script's own reads, with a status `accept` admits. */
async function readJson(
  url: string,
  headers: Record<string, string>,
  accept: (status: number) => boolean
): Promise<unknown> {
  const response = await fetch(url, {
    headers: { Accept: "application/json", ...headers },
    signal: AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS),
  });
  if (!accept(response.status)) throw new HttpStatusError(response.status);
  const text = await response.text();
  try {
    return JSON.parse(text);
  } catch {
    throw new MalformedAnswer();
  }
}

function liveDispatch(config: DispatchConfig): DispatchReads {
  const base = config.url.replace(/\/+$/, "");
  // A client's deadline runs from its construction, so each read gets a client of its own.
  const client = () => new DispatchClient(config.url, config.token);
  return {
    // Public: it is sent no token.
    healthz: () => readJson(`${base}/healthz`, {}, (status) => status === 200 || status === 503),
    search: (query, options) => client().search(query, options),
    issue: (key) => client().getIssue(key),
    coverage: () =>
      readJson(
        `${base}/api/v1/search/coverage`,
        { Authorization: `Bearer ${config.token}` },
        (status) => status >= 200 && status < 300
      ),
  };
}

/** The lexer over Bun's `SQL`, connected on first use to a Postgres URL or a socket path. */
function postgresLexer(target: string): Lexer & { close(): Promise<void> } {
  let sql: SQL | undefined;
  return {
    async lexemes(key, title, query) {
      if (sql === undefined) {
        // Bun refuses a URL naming a socket directory with `?host=`; the `path` form connects.
        sql = POSTGRES_URL.test(target)
          ? new SQL(target)
          : new SQL({ path: target, username: "postgres", database: "postgres" });
      }
      // The issue row's vector (`migrations/0088_issues_title_lexemes.up.sql`) and search's
      // query (`api/search.go`), every value bound.
      const [row] = LexemeRows.parse(
        await sql`
          select
            tsvector_to_array(
              to_tsvector('english', regexp_replace(${key}::text, ${SEARCH_TEXT_PATTERN}::text, ${SEARCH_TEXT_REPLACEMENT}::text, 'g'))
              || to_tsvector('english', regexp_replace(${title}::text, ${SEARCH_TEXT_PATTERN}::text, ${SEARCH_TEXT_REPLACEMENT}::text, 'g'))
            ) as issue,
            querytree(websearch_to_tsquery('english', ${query}::text)) as query
        `
      );
      return { issue: row.issue, query: queryTreeLexemes(row.query) };
    },
    async close() {
      await sql?.close().catch(() => undefined);
    },
  };
}

/** Dispatch as every command resolves it, or null when it is off. Its warnings name file paths,
 * so they are not printed. */
function resolveDispatch(): DispatchConfig | null {
  try {
    const { dispatch } = sessionDispatchSettings(
      sessionConfigFile("command", process.env),
      process.env,
      { cwd: process.cwd() }
    );
    return dispatch.enabled ? dispatch : null;
  } catch {
    return null;
  }
}

async function main(argv: readonly string[]): Promise<number> {
  let options: Options;
  try {
    options = parseArguments(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(`usage: ${error.message}`);
    return 2;
  }
  const config = resolveDispatch();
  if (config === null) {
    console.error("unavailable: config");
    return 2;
  }
  const lexer =
    options.lexemeDatabase === undefined ? undefined : postgresLexer(options.lexemeDatabase);
  try {
    return await runChecks(options, {
      dispatch: liveDispatch(config),
      ...(lexer === undefined ? {} : { lexer }),
      out: (line) => console.log(line),
      err: (line) => console.error(line),
    });
  } finally {
    await lexer?.close();
  }
}

if (import.meta.main) {
  let code: number;
  try {
    code = await main(Bun.argv.slice(2));
  } catch (error) {
    console.error(`unavailable: ${errorCategory(error)}`);
    code = 2;
  }
  process.exit(code);
}
