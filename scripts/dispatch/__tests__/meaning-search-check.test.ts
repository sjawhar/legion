import { describe, expect, test } from "bun:test";
import { DispatchGatewayError } from "../../../packages/envoy-client/src/dispatch-http.ts";
import {
  type Clock,
  type DispatchReads,
  exitCode,
  HttpStatusError,
  type Lexer,
  parseArguments,
  queryTreeLexemes,
  runChecks,
  UsageError,
} from "../meaning-search-check.ts";

const TITLE = "Quarterly vendor ledger reconciliation";
const QUERY = "money owed to suppliers stopped adding up";
const IDENTITY = "azure:private-resource.internal.example/cohere-embed-v4";
const HEALTH = {
  ok: true,
  db: true,
  nats: true,
  files: true,
  commit: "0123abcd",
  schema_version: 88,
};

function issueRow(key: string) {
  return { kind: "issue", owner: { kind: "issue", key, title: TITLE, status: "done" } };
}

function owned(kind: string, key: string) {
  return { kind, owner: { kind: "issue", key, title: TITLE, status: "done" } };
}

function projectDocument() {
  return { kind: "document", owner: { kind: "document", project: "ACME", slug: "notes" } };
}

function answer(results: readonly unknown[], extra: object = {}) {
  return { results, took_ms: 4, ...extra };
}

function counts(
  items: number,
  current: number,
  lacking: number,
  failed: number,
  oldest: number | null = null
) {
  return { items, current, lacking, failed, oldest_lacking_seconds: oldest };
}

function report(
  total = counts(10, 10, 0, 0),
  kinds: Record<string, unknown> = { issue: total },
  extra: object = {}
) {
  return { identity: IDENTITY, place: "azure", model: IDENTITY, total, kinds, ...extra };
}

function dispatch(overrides: Partial<DispatchReads> = {}): DispatchReads {
  return {
    healthz: async () => HEALTH,
    search: async () => answer([]),
    issue: async (key) => ({ key, title: TITLE }),
    coverage: async () => report(),
    ...overrides,
  };
}

/** A lexer whose issue row shares `shared` of the query's lexemes. */
function lexer(shared: string[] = []): Lexer {
  return {
    lexemes: async () => ({
      issue: ["quarter", "vendor", "ledger", ...shared],
      query: ["money", "owe", "supplier", ...shared],
    }),
  };
}

/** A clock that only moves when the check sleeps, recording each sleep. */
interface FakeClock extends Clock {
  readonly sleeps: number[];
}

function clock(): FakeClock {
  let now = 0;
  const sleeps: number[] = [];
  return {
    sleeps,
    now: () => now,
    sleep: async (ms) => {
      sleeps.push(ms);
      now += ms;
    },
  };
}

async function run(
  argv: string[],
  deps: { dispatch?: DispatchReads; lexer?: Lexer; clock?: FakeClock } = {}
) {
  const out: string[] = [];
  const err: string[] = [];
  const code = await runChecks(parseArguments(argv), {
    dispatch: deps.dispatch ?? dispatch(),
    ...(deps.lexer === undefined ? {} : { lexer: deps.lexer }),
    ...(deps.clock === undefined ? {} : { clock: deps.clock }),
    out: (line) => out.push(line),
    err: (line) => err.push(line),
  });
  return { code, out, err };
}

const COVERAGE = ["--coverage", "--expect-model", IDENTITY];

describe("build", () => {
  test("prints the commit and schema of a 503 body and counts as available", async () => {
    const unhealthy = { ...HEALTH, ok: false, db: false, schema_version: null };
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({ healthz: async () => unhealthy }),
    });
    expect(result.out[0]).toBe("build commit=0123abcd schema=null ok=false");
    expect(result.code).toBe(0);
  });

  test("a commit that is not a hex sha is malformed and unavailable", async () => {
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({ healthz: async () => ({ ...HEALTH, commit: "main; rm -rf" }) }),
    });
    expect(result.out[0]).toBe("build unavailable: malformed");
    expect(result.out.at(-1)).toBe("result unavailable exit=2");
    expect(result.code).toBe(2);
  });
});

describe("A1", () => {
  test("degraded present fails", async () => {
    const degraded = answer([issueRow("ACME-1")], { degraded: "embedder_unavailable" });
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({ search: async () => degraded }),
    });
    expect(result.out).toEqual([
      "build commit=0123abcd schema=88 ok=true",
      "A1 degraded=yes results=1 fail",
      "result fail exit=1",
    ]);
    expect(result.code).toBe(1);
  });

  test("degraded absent passes", async () => {
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({ search: async () => answer([issueRow("ACME-1"), projectDocument()]) }),
    });
    expect(result.out).toContain("A1 degraded=no results=2 pass");
    expect(result.out.at(-1)).toBe("result pass exit=0");
    expect(result.code).toBe(0);
  });

  test("a client error is unavailable", async () => {
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({
        search: async () => {
          throw new TypeError("fetch failed");
        },
      }),
    });
    expect(result.out).toContain("A1 unavailable: transport");
    expect(result.code).toBe(2);
  });

  test("a non-JSON 2xx the client answers as a string is malformed", async () => {
    const result = await run(["--a1-query", QUERY], {
      dispatch: dispatch({ search: async () => "<html>sign in</html>" }),
    });
    expect(result.out).toContain("A1 unavailable: malformed");
    expect(result.code).toBe(2);
  });
});

describe("A2", () => {
  const pair = `ACME-7=${QUERY}`;

  function searching(results: readonly unknown[]) {
    return dispatch({ search: async () => answer(results) });
  }

  test("the issue's own row at position 3 passes", async () => {
    const results = [projectDocument(), owned("comment", "ACME-7"), issueRow("ACME-7")];
    const result = await run(["--pair", pair], { dispatch: searching(results), lexer: lexer() });
    expect(result.out).toContain("A2 ACME-7 shared=0 position=3 pass");
    expect(result.out).toContain("A2 pass (pairs passed 1 of 1)");
    expect(result.code).toBe(0);
  });

  test("the target's own document at position 1 with no issue row fails", async () => {
    const results = [owned("document", "ACME-7"), owned("comment", "ACME-7")];
    const result = await run(["--pair", pair], { dispatch: searching(results), lexer: lexer() });
    expect(result.out).toContain("A2 ACME-7 shared=0 position=none fail");
    expect(result.out).toContain("A2 fail (pairs passed 0 of 1)");
    expect(result.code).toBe(1);
  });

  test("one shared lexeme fails even with the issue row at position 1", async () => {
    const result = await run(["--pair", pair], {
      dispatch: searching([issueRow("ACME-7")]),
      lexer: lexer(["vendor"]),
    });
    expect(result.out).toContain("A2 ACME-7 shared=1 position=1 fail");
    expect(result.code).toBe(1);
  });

  test("a lexer that throws leaves the pair unchecked and unavailable", async () => {
    const broken: Lexer = {
      lexemes: async () => {
        throw new Error("connect ECONNREFUSED /var/run/postgresql/.s.PGSQL.5432");
      },
    };
    const result = await run(["--pair", pair], {
      dispatch: searching([issueRow("ACME-7")]),
      lexer: broken,
    });
    expect(result.out).toContain(
      "A2 ACME-7 shared=unchecked position=1 unavailable: lexeme-database"
    );
    expect(result.out).toContain("A2 unavailable (pairs passed 0 of 1)");
    expect(result.code).toBe(2);
  });

  test("no lexer leaves the pair unchecked and unavailable", async () => {
    const result = await run(["--pair", pair], { dispatch: searching([issueRow("ACME-7")]) });
    expect(result.out).toContain(
      "A2 ACME-7 shared=unchecked position=1 unavailable: lexeme-database"
    );
    expect(result.code).toBe(2);
  });

  test("one passing pair of three passes A2 and every pair is printed", async () => {
    const byQuery: Record<string, unknown[]> = {
      "first query": [projectDocument()],
      "second query": [issueRow("ACME-2")],
      "third query": [owned("ask", "ACME-3")],
    };
    const result = await run(
      [
        "--pair",
        "ACME-1=first query",
        "--pair",
        "ACME-2=second query",
        "--pair",
        "ACME-3=third query",
      ],
      {
        dispatch: dispatch({ search: async (query) => answer(byQuery[query] ?? []) }),
        lexer: lexer(),
      }
    );
    expect(result.out).toEqual([
      "build commit=0123abcd schema=88 ok=true",
      "A2 ACME-1 shared=0 position=none fail",
      "A2 ACME-2 shared=0 position=1 pass",
      "A2 ACME-3 shared=0 position=none fail",
      "A2 pass (pairs passed 1 of 3)",
      "result pass exit=0",
    ]);
    expect(result.code).toBe(0);
  });

  test("competing results above the target push it out of the top five", async () => {
    const results = [
      issueRow("ACME-40"),
      owned("document", "ACME-7"),
      owned("comment", "ACME-41"),
      projectDocument(),
      owned("message", "ACME-7"),
      issueRow("ACME-7"),
    ];
    const result = await run(["--pair", pair], { dispatch: searching(results), lexer: lexer() });
    expect(result.out).toContain("A2 ACME-7 shared=0 position=none fail");
    expect(result.code).toBe(1);
  });

  test("fewer than five results with the target at position 2 passes", async () => {
    const results = [owned("comment", "ACME-9"), issueRow("ACME-7")];
    const result = await run(["--pair", pair], { dispatch: searching(results), lexer: lexer() });
    expect(result.out).toContain("A2 ACME-7 shared=0 position=2 pass");
    expect(result.code).toBe(0);
  });

  test("an issue row of another key at position 1 is not counted", async () => {
    const result = await run(["--pair", pair], {
      dispatch: searching([issueRow("ACME-70")]),
      lexer: lexer(),
    });
    expect(result.out).toContain("A2 ACME-7 shared=0 position=none fail");
    expect(result.code).toBe(1);
  });

  test("asks search for the query's top five alone", async () => {
    const requests: unknown[] = [];
    await run(["--pair", pair], {
      dispatch: dispatch({
        search: async (query, options) => {
          requests.push({ query, options });
          return answer([]);
        },
      }),
      lexer: lexer(),
    });
    expect(requests).toEqual([{ query: QUERY, options: { limit: 5 } }]);
  });

  test("a failed issue read is unavailable with its category", async () => {
    const result = await run(["--pair", pair], {
      dispatch: dispatch({
        search: async () => answer([issueRow("ACME-7")]),
        issue: async () => {
          throw new HttpStatusError(404);
        },
      }),
      lexer: lexer(),
    });
    expect(result.out).toContain("A2 ACME-7 shared=unchecked position=1 unavailable: not-found");
    expect(result.code).toBe(2);
  });

  test("a failed search prints the pair's category alone", async () => {
    const result = await run(["--pair", pair], {
      dispatch: dispatch({
        search: async () => {
          throw new HttpStatusError(403);
        },
      }),
      lexer: lexer(),
    });
    expect(result.out).toContain("A2 ACME-7 unavailable: auth");
    expect(result.code).toBe(2);
  });

  test("a result without an owner kind is malformed", async () => {
    const result = await run(["--pair", pair], {
      dispatch: searching([{ kind: "issue", owner: { key: "ACME-7" } }]),
      lexer: lexer(),
    });
    expect(result.out).toContain("A2 ACME-7 unavailable: malformed");
    expect(result.code).toBe(2);
  });
});

describe("queryTreeLexemes", () => {
  test("reads each quoted lexeme of a querytree, phrases included", () => {
    expect(queryTreeLexemes("'languag' & 'model' & 'autom' <-> 'code' & 'o' <-> 'brien'")).toEqual([
      "languag",
      "model",
      "autom",
      "code",
      "o",
      "brien",
    ]);
  });

  test("undoubles an escaped quote and backslash", () => {
    expect(queryTreeLexemes("'o''brien' & 'c\\\\d'")).toEqual(["o'brien", "c\\d"]);
  });

  test("a query with nothing indexable holds no lexeme", () => {
    expect(queryTreeLexemes("T")).toEqual([]);
    expect(queryTreeLexemes("")).toEqual([]);
  });

  test("a repeated lexeme counts once", () => {
    expect(queryTreeLexemes("'run' & 'run' <-> 'bot' & 'run'")).toEqual(["run", "bot"]);
  });
});

describe("A3", () => {
  function reading(...answers: (unknown | Error)[]): DispatchReads["coverage"] {
    let reads = 0;
    return async () => {
      const next = answers[Math.min(reads, answers.length - 1)];
      reads += 1;
      if (next instanceof Error) throw next;
      return next;
    };
  }

  async function coverage(answers: (unknown | Error)[], argv: string[] = COVERAGE, time = clock()) {
    return run(argv, { dispatch: dispatch({ coverage: reading(...answers) }), clock: time });
  }

  test("everything current passes on one read and prints each kind", async () => {
    const total = counts(10, 10, 0, 0);
    const result = await coverage([
      report(total, { issue: counts(4, 4, 0, 0), comment: counts(6, 6, 0, 0) }),
    ]);
    expect(result.out).toEqual([
      "build commit=0123abcd schema=88 ok=true",
      "A3 identity=match place=azure items=10 current=10 lacking=0 oldest_lacking_s=none failed=0 reads=1 pass",
      "A3 kind=issue items=4 current=4 lacking=0 oldest_lacking_s=none failed=0",
      "A3 kind=comment items=6 current=6 lacking=0 oldest_lacking_s=none failed=0",
      "result pass exit=0",
    ]);
    expect(result.code).toBe(0);
  });

  test("a failed item fails on the first read without waiting", async () => {
    const time = clock();
    const result = await coverage([report(counts(10, 9, 0, 1))], COVERAGE, time);
    expect(result.out[1]).toBe(
      "A3 identity=match place=azure items=10 current=9 lacking=0 oldest_lacking_s=none failed=1 reads=1 fail"
    );
    expect(time.sleeps).toEqual([]);
    expect(result.code).toBe(1);
  });

  test("an item lacking on every read fails once the wait runs out", async () => {
    const time = clock();
    const lacking = counts(10, 9, 1, 0, 3);
    const result = await coverage([report(lacking)], [...COVERAGE, "--coverage-wait", "20"], time);
    // Reads at 0, 5, 10, 15 and 20 seconds; a sixth, at 25, would be past the 20-second wait.
    expect(result.out[1]).toBe(
      "A3 identity=match place=azure items=10 current=9 lacking=1 oldest_lacking_s=3 failed=0 reads=5 fail"
    );
    expect(time.sleeps).toEqual([5000, 5000, 5000, 5000]);
    expect(result.code).toBe(1);
  });

  test("a wait of 0 reads once", async () => {
    const result = await coverage(
      [report(counts(10, 9, 1, 0))],
      [...COVERAGE, "--coverage-wait", "0"]
    );
    expect(result.out[1]).toEndWith("reads=1 fail");
  });

  test("zero items fails", async () => {
    const result = await coverage([report(counts(0, 0, 0, 0))]);
    expect(result.out[1]).toEndWith(
      "items=0 current=0 lacking=0 oldest_lacking_s=none failed=0 reads=1 fail"
    );
    expect(result.code).toBe(1);
  });

  test("another identity fails on one read and is never printed", async () => {
    const other = "us.cohere.embed-v4:0";
    const time = clock();
    const result = await coverage(
      [report(counts(10, 9, 1, 0), undefined, { identity: other, place: "bedrock" })],
      COVERAGE,
      time
    );
    expect(result.out[1]).toStartWith("A3 identity=mismatch place=bedrock ");
    expect(result.out[1]).toEndWith("reads=1 fail");
    expect(time.sleeps).toEqual([]);
    expect([...result.out, ...result.err].join("\n")).not.toContain(other);
    expect(result.code).toBe(1);
  });

  test("a null identity fails", async () => {
    const result = await coverage([
      report(undefined, undefined, { identity: null, place: null, model: null }),
    ]);
    expect(result.out[1]).toStartWith("A3 identity=mismatch place=none ");
    expect(result.code).toBe(1);
  });

  test("lacking on the first read and current on the second passes on two reads", async () => {
    const result = await coverage([report(counts(10, 9, 1, 0)), report(counts(10, 10, 0, 0))]);
    expect(result.out[1]).toEndWith("reads=2 pass");
    expect(result.code).toBe(0);
  });

  test.each([
    ["empty kinds", report(counts(10, 10, 0, 0), {})],
    ["a kind whose counts do not add up", report(undefined, { issue: counts(10, 9, 0, 0) })],
    [
      "a missing field",
      report(undefined, undefined, { total: { items: 10, current: 10, lacking: 0, failed: 0 } }),
    ],
    ["a place outside the enum", report(undefined, undefined, { place: "gcp" })],
    ["a kind name outside the pattern", report(undefined, { "Issue Kind": counts(10, 10, 0, 0) })],
  ])("%s is malformed", async (_name, body) => {
    const result = await coverage([body]);
    expect(result.out).toContain("A3 unavailable: malformed reads=1");
    expect(result.code).toBe(2);
  });

  test("a 404 on the first read is unavailable", async () => {
    const result = await coverage([new HttpStatusError(404)]);
    expect(result.out).toContain("A3 unavailable: not-found reads=1");
    expect(result.out.at(-1)).toBe("result unavailable exit=2");
    expect(result.code).toBe(2);
  });

  test("a 404 on the second read is unavailable", async () => {
    const result = await coverage([report(counts(10, 9, 1, 0)), new HttpStatusError(404)]);
    expect(result.out).toContain("A3 unavailable: not-found reads=2");
    expect(result.code).toBe(2);
  });

  test("a report listing other kinds passes when it otherwise holds", async () => {
    const result = await coverage([
      report(counts(7, 7, 0, 0), { block: counts(5, 5, 0, 0), ask_part: counts(2, 2, 0, 0) }),
    ]);
    expect(result.out).toContain(
      "A3 kind=block items=5 current=5 lacking=0 oldest_lacking_s=none failed=0"
    );
    expect(result.out).toContain(
      "A3 kind=ask_part items=2 current=2 lacking=0 oldest_lacking_s=none failed=0"
    );
    expect(result.code).toBe(0);
  });

  test("the identity is never printed", async () => {
    const result = await coverage([report()]);
    expect([...result.out, ...result.err].join("\n")).not.toContain(IDENTITY);
  });
});

describe("exitCode", () => {
  test("a failure without an unavailable check is 1", () => {
    expect(exitCode(["pass", "fail"])).toBe(1);
  });

  test("an unavailable check outranks a failure", () => {
    expect(exitCode(["fail", "unavailable"])).toBe(2);
  });

  test("every check passing is 0", () => {
    expect(exitCode(["pass", "pass", "pass"])).toBe(0);
  });
});

describe("output hygiene", () => {
  const HOST = "dispatch.example-tailnet.ts.net";
  const TOKEN = "dsp_9f8e7d6c5b4a39281706f5e4d3c2b1a0";
  const BODY = "upstream connect error: private-backend reset";

  function gatewayError(status: number) {
    return new DispatchGatewayError(
      status,
      `GET https://${HOST}/api/v1/search?q=${encodeURIComponent(QUERY)}&token=${TOKEN} answered ${status} with a body that is not Dispatch's error JSON ("${BODY}")`,
      "asking again may help"
    );
  }

  function failing(status: number): DispatchReads {
    const fail = async (): Promise<never> => {
      throw gatewayError(status);
    };
    return { healthz: fail, search: fail, issue: fail, coverage: fail };
  }

  const argv = ["--a1-query", QUERY, "--pair", `ACME-7=${QUERY}`, ...COVERAGE];

  test.each([
    [502, "transport"],
    [401, "auth"],
  ])("a %i gateway error prints only its category", async (status, category) => {
    const result = await run(argv, { dispatch: failing(status), lexer: lexer() });
    const printed = [...result.out, ...result.err].join("\n");
    for (const secret of [HOST, TOKEN, BODY, QUERY, "https://"]) {
      expect(printed).not.toContain(secret);
    }
    expect(result.out).toEqual([
      `build unavailable: ${category}`,
      `A1 unavailable: ${category}`,
      `A2 ACME-7 unavailable: ${category}`,
      "A2 unavailable (pairs passed 0 of 1)",
      `A3 unavailable: ${category} reads=1`,
      "result unavailable exit=2",
    ]);
    expect(result.code).toBe(2);
  });

  test("the issue title and the queries never appear", async () => {
    const result = await run(argv, {
      dispatch: dispatch({ search: async () => answer([issueRow("ACME-7")]) }),
      lexer: lexer(),
    });
    const printed = [...result.out, ...result.err].join("\n");
    expect(printed).toContain("A2 ACME-7 shared=0 position=1 pass");
    expect(printed).not.toContain(TITLE);
    expect(printed).not.toContain(QUERY);
  });
});

describe("parseArguments", () => {
  test("--coverage without --expect-model is refused", () => {
    expect(() => parseArguments(["--coverage"])).toThrow(UsageError);
  });

  test("--expect-model without --coverage is refused", () => {
    expect(() => parseArguments(["--a1-query", QUERY, "--expect-model", IDENTITY])).toThrow(
      UsageError
    );
  });

  test("no check flag is refused", () => {
    expect(() => parseArguments(["--lexeme-database", "/sock/.s.PGSQL.5432"])).toThrow(UsageError);
  });

  test("a malformed pair key is refused without repeating it", () => {
    const bad = "acme-7-private-words=some query";
    let caught: unknown;
    try {
      parseArguments(["--pair", bad]);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(UsageError);
    expect((caught as UsageError).message).not.toContain("acme-7-private-words");
    expect((caught as UsageError).message).not.toContain("some query");
  });

  test("an unknown option is refused without repeating it", () => {
    let caught: unknown;
    try {
      parseArguments(["--secret-host-name"]);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(UsageError);
    expect((caught as UsageError).message).not.toContain("secret-host-name");
  });

  test("--coverage-wait must be a whole number of seconds", () => {
    expect(() => parseArguments([...COVERAGE, "--coverage-wait", "1.5"])).toThrow(UsageError);
    expect(() => parseArguments([...COVERAGE, "--coverage-wait", "-1"])).toThrow(UsageError);
  });

  test("a pair splits at its first =", () => {
    expect(parseArguments(["--pair", "ACME-7=a=b"]).pairs).toEqual([
      { key: "ACME-7", query: "a=b" },
    ]);
  });
});
