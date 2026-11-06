/**
 * The unit suite's network, enabled by the preload (`setup.ts`), which imports this module after
 * `register-dom.ts` and never before it.
 *
 * A unit test has no Dispatch, so every request it makes is answered by something the test set up
 * or it fails that test, reads included: MSW resolves each request against the handlers this file
 * and each test file install, and a request no handler answers is recorded with its method and
 * path and the request itself is errored. The preload fails the test with what was recorded
 * (`requestsThatReachedNetwork`), so a read the page swallowed in a `catch` is reported all the
 * same, and one a retry makes after the file's last test is reported there too.
 *
 * Most of the suite answers a level above this one, at the api-client method
 * (`spyOn(api, "issue")`) or at the request identity `send-answers.ts` and `keyed-attempt.ts`
 * carry; MSW sits below both, so for those calls there is no request for it to see. What this
 * answers is what reaches `fetch` or `XMLHttpRequest`: the page's own reads of the routes a test
 * did not spy.
 *
 * Both transports are answered. `fetch` carries every request but one; an upload goes through
 * `XMLHttpRequest` for its progress events (`api/client.ts`, `xhrUpload`), and happy-dom's
 * `XMLHttpRequest` does not go through `globalThis.fetch`, so the two are intercepted apart. The
 * `/web` interceptors are the ones that reach this runtime: the default export of each is the
 * node build, which intercepts neither `fetch` nor `XMLHttpRequest` here.
 */

import { FetchInterceptor } from "@mswjs/interceptors/fetch/web";
import { XMLHttpRequestInterceptor } from "@mswjs/interceptors/XMLHttpRequest/web";
import { http, type RequestHandler } from "msw";
import { defineNetwork, InterceptorSource } from "msw/experimental";
import { TEST_ORIGIN } from "./register-dom";

/** Every request a test let reach the network since the last report, each `<METHOD> <path>`. */
const reached: string[] = [];

/**
 * Every request that reached the network since this was last called, and forgets them: the
 * preload's `afterEach` fails the test with them, so a request still retrying after a file's last
 * test is reported rather than lost with the test runner's exit.
 */
export function requestsThatReachedNetwork(): string[] {
  return reached.splice(0);
}

/**
 * The routes the application shell reads on every rendered page, answered with the API's empty
 * shape so that a page under test renders its loaded-empty state rather than its error state. A
 * test that asserts one of these answers spies the `api` method that fetches it, and such a test
 * never reaches a handler. Every other route is answered by the file that needs it
 * (`answerRequests`).
 */
const shellHandlers: RequestHandler[] = [
  // No identity, which is what a page with no `api.whoami` spy showed before MSW: the read failed
  // and the shell rendered signed out. This is Dispatch's own answer for a visitor it does not
  // know, status and code both.
  http.get("/auth/whoami", () =>
    Response.json({ code: "NO_IDENTITY", error: "no identity" }, { status: 401 })
  ),
  http.get("/api/v1/agents", () => Response.json([])),
  http.get("/api/v1/me/replies", () => Response.json({ rows: [], total: 0, limit: 50, offset: 0 })),
  http.get("/api/v1/credential-requests", () => Response.json({ pending: [] })),
  http.get("/api/v1/me/agents/state", () => Response.json({})),
];

const network = defineNetwork({
  sources: [
    new InterceptorSource({
      // The `/web` interceptors and msw's `InterceptorSource` take their `Interceptor` base from
      // two of the package's type entries, the browser one and the node one, so TypeScript reports
      // a private-brand mismatch between classes that are one class at run time.
      interceptors: [
        new FetchInterceptor(),
        new XMLHttpRequestInterceptor(),
      ] as unknown as ConstructorParameters<typeof InterceptorSource>[0]["interceptors"],
    }),
  ],
  handlers: shellHandlers,
  context: { baseUrl: TEST_ORIGIN, quiet: true },
  // msw 3's `defaults.error()` only prints its message - it does not error the frame - and a frame
  // no handler resolved is performed for real, so the request is errored by throwing from here.
  onUnhandledFrame: ({ frame }) => {
    const { request } = frame.data as { request: Request };
    const named = `${request.method} ${new URL(request.url).pathname}`;
    reached.push(named);
    throw new Error(`a unit test's request reached the network, answered by nothing: ${named}`);
  },
});

network.enable();

/** The routes this test answers, for the rest of it: the preload forgets them after each test. */
export function answerRequests(...handlers: RequestHandler[]): void {
  network.use(...handlers);
}

/** Forgets every route a test answered, leaving the shell's: the preload's `afterEach`. */
export function forgetTestAnswers(): void {
  network.resetHandlers();
}
