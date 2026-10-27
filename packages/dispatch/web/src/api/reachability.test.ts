import { expect, test } from "bun:test";

import { onReachable, type Reachable, reachable, routeOf, unreachable } from "./reachability";

/** What the page was told Dispatch can be reached again, each `[how, route]`, while `body` runs. */
function learned(body: () => void): [Reachable, string | undefined][] {
  const told: [Reachable, string | undefined][] = [];
  const stop = onReachable((how, route) => told.push([how, route]));
  try {
    body();
  } finally {
    stop();
  }
  return told;
}

// W60. Only a write waits to learn its route can be reached again: a read's failure is recorded by
// nothing. Every read of a page is a route of its own - a path and its query string - so recording
// them would grow the set for the page's whole life while nothing ever read it, and a read's later
// answer would tell the listeners of news no write asked for.
test("a read's failed route is not recorded, so its answer tells no one", () => {
  const told = learned(() => {
    unreachable(routeOf("GET", "/api/v1/issues?project=CORE"));
    reachable("answer", routeOf("GET", "/api/v1/issues?project=CORE"));
    unreachable(routeOf("HEAD", "/api/v1/issues/CORE-1"));
    reachable("answer", routeOf("HEAD", "/api/v1/issues/CORE-1"));
  });
  expect(told).toEqual([]);
});

// W60. A write's route is recorded until it answers, and forgotten as it does: the next answer
// there is no news, since nothing failed on that route since.
test("a write's route is told once it answers, and is forgotten as it does", () => {
  const route = routeOf("PATCH", "/api/v1/issues/CORE-1");
  const told = learned(() => {
    reachable("answer", route);
    unreachable(route);
    reachable("answer", route);
    reachable("answer", route);
  });
  expect(told).toEqual([["answer", route]]);
});

// A route is a method and a path: an answer on one route says nothing of another's, and each is
// kept and forgotten on its own.
test("each write route is kept on its own", () => {
  const patch = routeOf("PATCH", "/api/v1/issues/CORE-1");
  const post = routeOf("POST", "/api/v1/issues/CORE-1/comments");
  const told = learned(() => {
    unreachable(patch);
    unreachable(post);
    reachable("answer", post);
    reachable("answer", patch);
  });
  expect(told).toEqual([
    ["answer", post],
    ["answer", patch],
  ]);
});

// The live socket is no route: it is lost and opens again on its own, and its opening is news for
// every waiting write only after it was lost.
test("the socket opening is news only after it was lost", () => {
  const told = learned(() => {
    reachable("socket");
    unreachable();
    reachable("socket");
    reachable("socket");
  });
  expect(told).toEqual([["socket", undefined]]);
});
