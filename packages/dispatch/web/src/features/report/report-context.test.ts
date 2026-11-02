import { expect, test } from "bun:test";

import { reportContext } from "./report-context";

test("a report names the page, its build, the browser and the window", () => {
  const view = {
    innerHeight: 800.4,
    innerWidth: 1280,
    location: { href: "https://dispatch.example/issues/CORE-1#spec" },
    navigator: { userAgent: "Mozilla/5.0 Test" },
  } as unknown as Window;
  expect(reportContext(view, () => "/assets/index-abc123.js")).toEqual({
    page: "https://dispatch.example/issues/CORE-1#spec",
    page_build: "/assets/index-abc123.js",
    user_agent: "Mozilla/5.0 Test",
    viewport: { height: 800, width: 1280 },
  });
});
