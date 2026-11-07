/**
 * What a `view` block's page cannot do (dispatch://INFRA-682, A4, R5): it runs in an opaque
 * origin, it reaches no network, it reads nothing of the reader, it is given a height only for
 * its own frame, and a page that moves its frame away is sent nothing more and shown as such.
 * A browser that opens the page's address in a tab is answered an attachment, not a page.
 *
 * Each row that asks whether a page reached the network starts a recording server of its own
 * (`e2e/recording-origin.ts`) on a second loopback origin, which every one of these engines
 * reaches. The first row runs the same fixture page twice: once as the test's own page with no
 * sandbox, where it does reach that server, and once as a `view` block's page, where it reaches
 * nothing. Without the first run, "the server saw no request" would also be what an unreachable
 * server looks like.
 *
 * The rows run in `chromium`, `firefox`, `webkit` and `webkit-iphone`: a sandbox, an opaque
 * origin and a Content-Security-Policy are each engine's own. Every row here carries `@webkit`
 * and `@firefox`, the tags those two projects select by, and the phrase `a view block`, which
 * `webkit-iphone`'s title `grep` selects by.
 */
import { expect, test } from "@playwright/test";

import { createProjectDocument, uploadIssueFile } from "./api";
import { startRecordingOrigin } from "./recording-origin";
import { resetDatabase } from "./seed";
import { asUser } from "./users";
import {
  attempts,
  seedViewIssue,
  seedViewProject,
  type ViewFixture,
  viewBlock,
  viewDirective,
} from "./view-fixtures";

/** The data a block names, so its attributes read as a view at all. */
const DATA = attempts(7);

/**
 * The fixture page of the first row: it tries, in order, to read the reader's session and
 * storage, to reach Dispatch and the recording server, to open a window, to submit a form and to
 * move the tab, and writes what each attempt answered into its own `#results`. The form posts
 * into a frame of the page's own rather than a new tab, so a popup blocker cannot be read as the
 * sandbox. Moving the tab goes last, because outside the sandbox it carries the page away.
 */
function probePage(recorder: string): string {
  return `<!doctype html>
<html lang="en"><body>
<pre id="results">running</pre>
<iframe name="sink" title="where the form posts" hidden></iframe>
<script>
(async () => {
  const recorder = ${JSON.stringify(recorder)};
  const tried = { violations: [] };
  const show = () => {
    document.getElementById("results").textContent = JSON.stringify(tried);
  };
  addEventListener("securitypolicyviolation", (event) => {
    tried.violations.push(event.effectiveDirective || event.violatedDirective);
    show();
  });
  const safely = (name, run) => {
    try {
      tried[name] = String(run());
    } catch (error) {
      tried[name] = "refused";
    }
    show();
  };
  const safelyAsync = async (name, run) => {
    try {
      tried[name] = String(await run());
    } catch (error) {
      tried[name] = "refused";
    }
    show();
  };

  tried.origin = String(window.origin);
  safely("cookie", () => (document.cookie === "" ? "empty" : document.cookie));
  safely("localStorage", () => {
    window.localStorage.setItem("probe", "1");
    return "read";
  });
  safely("parentDocument", () => String(parent.document.title));
  await safelyAsync("fetchDispatch", async () => "status " + (await fetch("/api/v1/whoami")).status);
  await safelyAsync(
    "fetchRecorder",
    async () => "status " + (await fetch(recorder + "/probe/fetch")).status
  );
  await safelyAsync("image", () => new Promise((resolve) => {
    const picture = new Image();
    picture.onload = () => resolve("loaded");
    picture.onerror = () => resolve("refused");
    picture.src = recorder + "/probe/image.png";
  }));
  safely("windowOpen", () => String(window.open(recorder + "/probe/open", "_blank")));
  safely("form", () => {
    const form = document.createElement("form");
    form.method = "POST";
    form.action = recorder + "/probe/form";
    form.target = "sink";
    document.body.appendChild(form);
    form.submit();
    return "submitted";
  });
  await new Promise((resolve) => setTimeout(resolve, 500));
  tried.done = true;
  show();
  safely("topLocation", () => {
    top.location = recorder + "/probe/top";
    return "moved";
  });
})();
</script>
</body></html>
`;
}

/** What the probe page wrote into `#results`. */
interface ProbeResults {
  cookie: string;
  done?: boolean;
  fetchDispatch: string;
  fetchRecorder: string;
  form: string;
  image: string;
  localStorage: string;
  origin: string;
  parentDocument: string;
  topLocation?: string;
  violations: string[];
  windowOpen: string;
}

test.beforeEach(async () => {
  await resetDatabase();
});

/** An issue holding one page file and one granted data file, both named after `name`, since
 *  one row seeds two pages and must tell their references apart. */
function seedPage(name: string, html: string, data = DATA): Promise<ViewFixture> {
  return seedViewIssue({
    data,
    dataName: name,
    html,
    pageName: name,
    title: `Page ${name}`,
  });
}

test("a view block's page reads nothing of the reader and reaches no network @webkit @firefox", async ({
  browser,
}) => {
  await seedViewProject();
  const recorder = await startRecordingOrigin();
  const alice = await asUser(browser, "alice");
  try {
    const html = probePage(recorder.origin);
    // The control: the same page, opened by the test with no sandbox and no Dispatch around it.
    // The recording origin serves it, so the attempts it makes on that origin are the page's
    // own origin's and no CORS rule stands in for the sandbox. It reaches the server, so a
    // server that sees nothing below sees nothing because of the sandbox.
    recorder.serve("/control", html);
    const control = await alice.newPage();
    await control.goto(`${recorder.origin}/control`);
    await expect
      .poll(() => recorder.paths(), {
        message: "A4 control: with no sandbox the page reaches the recording server",
        timeout: 20_000,
      })
      .toEqual(
        expect.arrayContaining(["/probe/fetch", "/probe/image.png", "/probe/form", "/probe/top"])
      );
    await control.close();
    recorder.reset();

    const fixture = await seedPage("probe", html);
    const document = await createProjectDocument("CORE", {
      content: viewDirective({
        caption: "What the page may do.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
      name: "Sandbox",
    });
    const page = await alice.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);

    const results = viewBlock(page, "What the page may do.")
      .frameLocator("iframe")
      .locator("#results");
    await expect(results, "A4: the page ran and reported every attempt").toContainText(
      '"done":true',
      {
        timeout: 30_000,
      }
    );
    const tried = JSON.parse(await results.innerText()) as ProbeResults;

    expect(tried.origin, "A4: the page reads its own origin as null").toBe("null");
    expect(tried.cookie, "A4: it reads no cookie of the reader").toMatch(/^(refused|empty)$/);
    expect(tried.cookie, "A4: and never the session cookie").not.toContain("dsession");
    expect(tried.localStorage, "A4: it reads no storage").toBe("refused");
    expect(tried.parentDocument, "A4: it reads nothing of the page around it").toBe("refused");
    expect(tried.fetchDispatch, "A4: it cannot call Dispatch").toBe("refused");
    expect(tried.fetchRecorder, "A4: it cannot fetch another origin").toBe("refused");
    expect(tried.image, "A4: it cannot load a picture from another origin").toBe("refused");
    expect(tried.windowOpen, "A4: it cannot open a window").toMatch(/^(null|refused)$/);
    expect(tried.topLocation, "A4: it cannot move the reader's tab").toBe("refused");
    expect(tried.form, "A4: it submitted a form, which reached nothing").toBe("submitted");
    expect(
      recorder.requests(),
      "A4: the recording server got no request from the sandboxed page"
    ).toEqual([]);
  } finally {
    await recorder.close();
    await alice.close();
  }
});

test("a view block takes a height from its own frame alone @webkit @firefox", async ({
  browser,
}) => {
  await seedViewProject();
  // A page that posts the height its data names, and nothing when the data names no number.
  const reporting = `<!doctype html>
<html lang="en"><body><p id="words">waiting</p><script>
window.addEventListener("message", (event) => {
  if (event.source !== window.parent) { return; }
  const message = event.data;
  if (message === null || typeof message !== "object" || message.type !== "dispatch-view:port") {
    return;
  }
  event.ports[0].onmessage = (sent) => {
    if (sent.data === null || sent.data.type !== "dispatch-view:data") { return; }
    const text = sent.data.files[0] && sent.data.files[0].text;
    document.getElementById("words").textContent = String(text);
    const height = Number(text);
    if (Number.isFinite(height) && height > 0) {
      parent.postMessage({ height: height, type: "dispatch-view:height" }, "*");
    }
  };
});
parent.postMessage({ type: "dispatch-view:ready" }, "*");
</script></body></html>
`;
  const quiet = await seedPage("quiet", reporting, "none");
  const loud = await seedPage("loud", reporting, "700");
  const alice = await asUser(browser, "alice");
  try {
    const document = await createProjectDocument("CORE", {
      content: [
        viewDirective({
          caption: "The block that posts no height.",
          data: quiet.dataRef,
          page: quiet.pageRef,
        }),
        viewDirective({
          caption: "The block that posts 700.",
          data: loud.dataRef,
          page: loud.pageRef,
        }),
      ].join("\n"),
      name: "Heights",
    });
    const page = await alice.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);

    const first = viewBlock(page, "The block that posts no height.");
    const second = viewBlock(page, "The block that posts 700.");
    await expect(first.frameLocator("iframe").locator("#words")).toHaveText("none");
    await expect(second.frameLocator("iframe").locator("#words")).toHaveText("700");
    await expect
      .poll(async () => Math.round((await second.locator("iframe").boundingBox())?.height ?? 0), {
        message: "A4: the block whose page posted 700 is 700 pixels high",
        timeout: 10_000,
      })
      .toBe(700);
    expect(
      Math.round((await first.locator("iframe").boundingBox())?.height ?? 0),
      "A4: and the block whose page posted nothing is unchanged"
    ).toBe(400);
  } finally {
    await alice.close();
  }
});

test("a view block whose page moves away from its address is sent nothing more @webkit @firefox", async ({
  browser,
}) => {
  await seedViewProject();
  // The address the page moves to reports any message it gets, by loading a picture the
  // recording server counts.
  const movedPage =
    '<!doctype html><html lang="en"><body><p>Moved.</p><script>' +
    'addEventListener("message", () => { new Image().src = "/probe/message.png"; });' +
    "</script></body></html>";
  const recorder = await startRecordingOrigin({ "/moved": movedPage });
  const alice = await asUser(browser, "alice");
  try {
    const wanderer = `<!doctype html>
<html lang="en"><body><p>Off we go.</p><script>
parent.postMessage({ type: "dispatch-view:ready" }, "*");
setTimeout(() => { window.location.href = ${JSON.stringify(`${recorder.origin}/moved`)}; }, 500);
</script></body></html>
`;
    const fixture = await seedPage("wanderer", wanderer);
    const document = await createProjectDocument("CORE", {
      content: viewDirective({
        caption: "A page that will not stay.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
      name: "Wanderer",
    });
    const page = await alice.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);

    const block = viewBlock(page, "A page that will not stay.");
    await expect(
      block.getByText(`${fixture.pageRef} — the page moved away from its address`),
      "A4: the block says the page moved away"
    ).toBeVisible({ timeout: 30_000 });
    await expect(block.locator("iframe"), "A4: and the frame is gone").toHaveCount(0);

    await uploadIssueFile(fixture.issue, {
      content: '[{"label":"attempts","value":9}]',
      name: "wanderer.json",
      type: "application/json",
    });
    await page.waitForTimeout(6_000);
    expect(
      recorder.paths().filter((path) => path === "/probe/message.png"),
      "A4: nothing more was sent to the page that moved away"
    ).toEqual([]);
    expect(
      recorder.paths().filter((path) => path === "/moved"),
      "A4: the page did move, once"
    ).toEqual(["/moved"]);
    await expect(block.locator("iframe"), "A4: and the block starts no new frame").toHaveCount(0);
  } finally {
    await recorder.close();
    await alice.close();
  }
});

/**
 * The sibling row above rests on the frame's second `load`, which is what sets "moved away". A
 * page that moves its frame to a document the other server answers and never finishes fires no
 * second `load` at all: the block's load count stays at one, no line appears, and the frame
 * stays where it is. So the data channel itself has to be the document's, and it is - Dispatch
 * transfers a `MessagePort` with its answer to the page's first `ready` and sends every data
 * message over that port alone. A port belongs to the document that received it, so the
 * navigation discards it with that document and the other server's document has nothing to
 * receive on. Posting to `frame.contentWindow` instead would reach it, whatever the origin,
 * since the page's opaque origin leaves `"*"` as the only target origin to name.
 *
 * The document moved to is alive and listening: it asks the recording server for
 * `/probe/alive.png` as its script runs and for `/probe/message.png` on any message it gets, so
 * "no message arrived" here is not "no document ran". Its `<img>` names a path the server
 * records and never answers, which is what holds its `load` event back for the whole row.
 */
test("a view block whose page moves to a document that never loads is sent nothing more @webkit @firefox", async ({
  browser,
}) => {
  await seedViewProject();
  // The document's own response is what never ends: its head and its body go, its script runs,
  // and `load` waits on a body that never closes. Two earlier forms of this row failed in
  // WebKit and the trace of each said why (dispatch://INFRA-682, R5). The first hung a trailing
  // `<img>`, which WebKit never requested, so the document finished there. The second streamed
  // this body unpadded: WebKit's trace shows it did request `/stuck` and held the answer open
  // for 41.9 s with no body length, so the navigation committed - but it ran no script, because
  // an HTML parser reading a streamed response waits for a buffer's worth before it parses at
  // all. The padding below is that buffer, so the script is parsed and run from the first flush
  // in every engine; nothing here asks an engine for anything but to parse what it was sent.
  const stuckPage =
    '<!doctype html><html lang="en"><body><p>Still loading.</p><script>' +
    'addEventListener("message", () => { new Image().src = "/probe/message.png"; });' +
    'new Image().src = "/probe/alive.png";' +
    "</script>" +
    `<!--${"padding so a streaming parser flushes this far. ".repeat(400)}-->` +
    "</body></html>";
  const recorder = await startRecordingOrigin();
  recorder.serveUnfinished("/stuck", stuckPage);
  const alice = await asUser(browser, "alice");
  try {
    // It moves on the answer to its `ready`, whatever that answer carries, so this page is the
    // same page before and after the port: Dispatch answers a ready page, and it goes.
    const wanderer = `<!doctype html>
<html lang="en"><body><p>Off we go.</p><script>
let going = false;
window.addEventListener("message", (event) => {
  if (event.source !== window.parent || going) { return; }
  going = true;
  window.location.href = ${JSON.stringify(`${recorder.origin}/stuck`)};
});
parent.postMessage({ type: "dispatch-view:ready" }, "*");
</script></body></html>
`;
    const fixture = await seedPage("stuck", wanderer);
    const document = await createProjectDocument("CORE", {
      content: viewDirective({
        caption: "A page that stops halfway.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
      name: "Stuck",
    });
    const page = await alice.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);

    const block = viewBlock(page, "A page that stops halfway.");
    await expect
      .poll(() => recorder.paths().filter((path) => path === "/probe/alive.png").length, {
        message: "A4: the document the page moved to is running and listening",
        timeout: 30_000,
      })
      .toBe(1);
    expect(
      recorder.openResponses("/stuck"),
      "A4: and it is still loading, because the server has not ended its response"
    ).toBe(1);

    await uploadIssueFile(fixture.issue, {
      content: '[{"label":"attempts","value":9}]',
      name: "stuck.json",
      type: "application/json",
    });
    await page.waitForTimeout(6_000);
    expect(
      recorder.paths().filter((path) => path === "/probe/message.png"),
      "A4: nothing was sent to the document the page moved to"
    ).toEqual([]);
    await expect(
      block.getByText(`${fixture.pageRef} — the page moved away from its address`),
      "A4: and no second load said the page moved away, which is why the port carries the data"
    ).toHaveCount(0);
  } finally {
    await recorder.close();
    await alice.close();
  }
});

test("a view block's page address opened in a tab is a download and no page @webkit @firefox", async ({
  browser,
}) => {
  await seedViewProject();
  const shown = `<!doctype html>
<html lang="en"><body><p id="words">a page in its frame</p><script>
parent.postMessage({ type: "dispatch-view:ready" }, "*");
</script></body></html>
`;
  const fixture = await seedPage("shown", shown);
  const alice = await asUser(browser, "alice");
  try {
    const document = await createProjectDocument("CORE", {
      content: viewDirective({
        caption: "A page a frame loaded.",
        data: fixture.dataRef,
        page: fixture.pageRef,
      }),
      name: "Download",
    });
    const page = await alice.newPage();
    await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
    const block = viewBlock(page, "A page a frame loaded.");
    const frame = block.locator("iframe");
    await expect(frame).toBeVisible();
    await expect(block.frameLocator("iframe").locator("#words")).toHaveText("a page in its frame");
    const address = await frame.getAttribute("src");
    expect(address, "A4: the frame has the page route's address").toContain("/page");

    // The same browser context, which has just loaded that address in a frame, opens it in a
    // tab. The answer that navigation gets is the same in every engine: `attachment`, and a
    // `Content-Type` of `application/octet-stream`, which no browser renders, so every engine
    // saves the bytes and none draws them (`pageHeaders`,
    // `internal/dispatch/api/artifact_files.go`). The type carries the rule rather than the
    // disposition alone, because a disposition is advice an engine may decline: WebKit displays
    // an attachment it believes it can render, so this row was red there while the page was
    // served `text/html` (sjawhar/legion#2647). The frame's own load above is the control: the
    // same address, with `Sec-Fetch-Dest: iframe`, is `inline` and `text/html` and did show its
    // page, so both headers here are the route switching on the request and not constants.
    const tab = await alice.newPage();
    const answer = tab.waitForResponse((response) => new URL(response.url()).pathname === address);
    const downloaded = tab.waitForEvent("download", { timeout: 15_000 }).catch(() => undefined);
    await tab.goto(address ?? "").catch(() => undefined);
    const headers = (await answer).headers();
    expect(headers["content-disposition"], "A4: the address in a tab is an attachment").toBe(
      "attachment"
    );
    expect(headers["content-type"], "A4: of a type no browser renders").toBe(
      "application/octet-stream"
    );

    const download = await downloaded;
    expect(download, "A4: the address in a tab is a download").not.toBe(undefined);
    expect(download?.suggestedFilename(), "A4: with a name to save it under").not.toBe("");
    await expect(tab.locator("#words"), "A4: and no page is shown").toHaveCount(0);
    await download?.delete();
    await tab.close();
  } finally {
    await alice.close();
  }
});
