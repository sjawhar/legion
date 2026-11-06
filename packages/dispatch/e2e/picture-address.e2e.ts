import { expect, type Page, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import { createIssue, createProject, dashboardOrigin } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

// A version's bytes route is an address a reader may be sent to on its own - a link pasted into
// the address bar, a picture opened in its own tab - and a browser sent there either draws the
// file where the reader is or hands it to them as a download. Dispatch answers a picture and a
// video `Content-Disposition: inline` with the artifact's own name, so the browser draws them;
// the Download control, which asks for the same bytes, still saves a file, and the file it saves
// carries that name rather than the version number its address ends with.
//
// Every helper here is this file's own: the file is run against a build of the change's merge
// base to see its rows fail, where a helper this change added would not load at all.

/** A 2x2 PNG, so the picture the browser draws has pixels of its own to report. */
const TINY_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFUlEQVR4nGP8z4AATAwMDAwMDAwMAA3+AR6vmgTYAAAAAElFTkSuQmCC";

/** A 32x32 WebM of a third of a second, VP8 video and Vorbis audio, which Chromium decodes: the
 *  player it draws reports a duration of its own. */
const TINY_WEBM = [
  "GkXfo59ChoEBQveBAULygQRC84EIQoKEd2VibUKHgQJChYECGFOAZwEAAAAAAAwFEU2bdLpNu4tTq4QVSalmU6yBoU27i1",
  "OrhBZUrmtTrIHWTbuMU6uEElTDZ1OsggpqTbuMU6uEHFO7a1Osggvv7AEAAAAAAABZAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAVSa",
  "lmsCrXsYMPQkBNgIxMYXZmNjEuMS4xMDBXQYxMYXZmNjEuMS4xMDBEiYhAdMAAAAAAABZUrmtJjq4BAAAAAAAAONeBAXPF",
  "iE+lHjqpG+5ZnIEAIrWcg3VuZIiBAIaFVl9WUDiDgQEj44OEBfXhAOCJsIEguoEgmoECrgEAAAAAAAlE14ECc8WIr/qx7T",
  "mCiBGcgQAitZyDdW5kiIEAhohBX1ZPUkJJU4OBAuGRn4EBtYhAv0AAAAAAAGJkgSBjokkFAh5cAXZvcmJpcwAAAAABQB8A",
  "AP////9AHwAA/////5kBA3ZvcmJpczQAAABYaXBoLk9yZyBsaWJWb3JiaXMgSSAyMDIwMDcwNCAoUmVkdWNpbmcgRW52aX",
  "Jvbm1lbnQpAQAAABQAAABlbmNvZGVyPUxhdmM2MS4zLjEwMAEFdm9yYmlzD0JDVgEAAAEADFIUISUZU0pjCJVSUikFHWNQ",
  "W0cdY9Q5RiFkEFOISRmle08qlVhKyBFSWClFHVNMU0mVUpYpRR1jFFNIIVPWMWWhcxRLhkkJJWxNrnQWS+iZY5YxRh1jzl",
  "pKnWPWMUUdY1JSSaFzGDpmJWQUOkbF6GJ8MDqVokIovsfeUukthYpbir3XGlPrLYQYS2nBCGFz7bXV3EpqxRhjjDHGxeJT",
  "KILQkFUAAAEAAEAEAUJDVgEACgAAwlAMRVGA0JBVAEAGAIAAFEVxFMdxHEeSJMsCQkNWAQBAAAACAAAojuEokiNJkmRZlm",
  "VZlqZ5lqi5qi/7ri7rru3qug6EhqwEAIAAABhGoXVMKoMQQ8pDhBRj0DOjEEMMTMYcY040pAwyiDPFkDKIWywuqBAEoSEr",
  "AoAoAADAGMQYYgw556RkUiLnmJROSgOdo9RRyiiVFEuMGaUSW4kxgs5R6ihllEKMpcWMUomxxFQAAECAAwBAgIVQaMiKAC",
  "AKAIAwBimFlEKMKeYUc4gx5RyDDDHGIGTOKegYk05K5ZyTzkmJGGPOMQeVc05K56Ry0EnJpBMAABDgAAAQYCEUGrIiAIgT",
  "ADBIkqZZmiaKlqaJomeKquqJoqpanmeanmmqqmeaqmqqquuaqurKlueZpmeaquqZpqqKpuq6pqq6rqeqtmy6qi6brmrbru",
  "z6tmu7vu6pqmybqivrpurKuurKtu66tu1Lnqeqoqq6rmeqrqu6rm6rrmvbmmrKrqm6sm26ri27smzrrizrumaqrmy6qi2b",
  "rizrruzatirLui+6rm6rsqz7qiz7vmzruu7auvCLrmvrqizrvirLvjHbtvDLui4ckyeqqqeqruuZquuqrmvbquvauqaarm",
  "y6ri2bqivbqizrtivLtq6ZqiybrivbpuvKsirLvu/Ksm6LruvrpizruirLwu7qujHMtq37ouvquirLuq/Ksq67uu77sm4L",
  "u6aqum7Ksq+bsqz7tq4Ly6zbujG6ru+rsi38qiwLv677wjLrPmN0XV9XbVkYVtn2fd33lWPWdWFZbVv5XVtnvL5uDLtu/M",
  "qtC8uy2raxzLqtLK+vG8Mu7Hxb+JWaqtq26bq6bsqyr8u6LrR13VdG1/V91bZ9X5Vl37eFX2kbw7CMrqv7qizrwmvLyi/r",
  "urDswi8sq20rv6vryjDburDcvrAsv+4Ly6rbvu/qutK1dWW5fZ+xK7fxCwAAGHAAAAgwoQwUGrIiAIgTAEAQcg4pBqFiCk",
  "IIoaQQQioVY1Iy5qRkzkkppZQUSkmtYkxK5pyUzDEpoZSWSimphFJaKqXEFEppLaXWYkqpxVBKa6Wk1kpJraWUYkytxRgx",
  "JiVzTkrmnJSSUmslldYy5yhlDkrqIKSUSiqtpNRi5pykDjorHYTUSioxlZRiC6nEVkpqraQUYysx1dRajiGlGEtKsZWUWm",
  "0x1dZaqzViTErmnJTMOSolpdZKKq1lzknqILTUOSippNRiKinFyjlJHYSUMsiolJRaK6nEElKJrbQUYympxdRirinFFkNJ",
  "LZaUWiypxNZijLW1VFMnpcWSUowllRhbrLm21moMpcRWSouxpJRbazHXFmOOoaQWSyuxlZRabLXl2FrLNbVUY0qt1hZjjT",
  "HllGutPafWYk0x1dparLnVllvMtedOSmullBZLSjG21mKNMeYcSmmtpBRbKSnG1lqtrcVcQymxldJaLKnE2GKstcVWY2qt",
  "xhZbraW1WmutvcZWWy6t1dxirD21lGusteZYU20FAAAMOAAABJhQBgoNWQkARAEAAMYwxhiERinHnJPSKOWcc1Iq5yCEkF",
  "LmHIQQUsqcg1BKS5lzEEpJKZSSUmqthVJSaq21AgAAChwAAAJs0JRYHKDQkJUAQCoAgMFxNE0UVdV1fV+xLFFUVdeVbeNX",
  "LE0UVVV2bVv4NVFUVde1bdsWfk0UVVV2Zdm2haKqurJt27JuC8Ooqq5r27Jt66iuq9u6rdu6L1RdWZZtW7d1Hde2dd32dV",
  "34GbNt67Zu677wIwxH3/gh5OP7dEIIAABPcAAAKrBhdYSTorHAQkNWAgAZAACAMUoZhRgzSDGmGGNMMcaYAACAAQcAgAAT",
  "ykChISsCgCgAAMA555xzzjnnnHPOOeecc84555xzjjHGGGOMMcYYY4wxxhhjjDHGGGOMMcYYY4wxxhhjTADAToQDwE6EhV",
  "BoyEoAIBwAAEAIISkppZRSShFTzkFJKaWUUqoUg4xKSimllFKkFHWUUkoppZQipaCkklJKKaWUSUkppZRSSimljDpKKaWU",
  "UkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppZRSSimllFJKKaWUUkoppVRKKaWUUk",
  "oppZRSSimlFABg8uAAAJVg4wwrSWeFo8GFhqwEAHIDAACFGIMQQmmtpFRSSiVVzkEoJZSUSkoplZRSqpiDEEoqqaWSUkop",
  "tdJBKKGUUEopJZRSSiglhBBKCaGUVEIrqYRSSgehhBJCKaGEVEopJZTOQSghhQ5CSaWU1EJIHXRUUikhlVJKKSWllDoIoZ",
  "SSUkstlVJaSql0ElIpqZXUUmqptZJSCaGkVkpJJaXSWkkltRJKSSWllFJLKYVUUkklhJJSKiW1llpKqbXWUkiplZRSSqml",
  "1FJKJaWQSkqplJJSaiWVlFJqIZWUSkkppNRKKaWkVEJJqaVSWkottZRKSam0VFJJpZSUSkkppVJKSymlEkpKqaWUWkkphZ",
  "JSSimVklJLJbVUSgolpZRSSam0lFJLKZWSUgEAQAcOAAABRlRaiJ1mXHkEjihkmIACAABAAECACSAwQFAwCkGAMAIBAAAA",
  "AMAAAB8AAEcBEBHRnMEBQoLCAkODwwMAAAAAAAAAAAAAAIASVMNnQNVzc59jwIBnyJlFo4dFTkNPREVSRIeMTGF2ZjYxLj",
  "EuMTAwc3PVY8CLY8WIT6UeOqkb7llnyKBFo4dFTkNPREVSRIeTTGF2YzYxLjMuMTAwIGxpYnZweGfIoUWjiERVUkFUSU9O",
  "RIeTMDA6MDA6MDAuMzMyMDAwMDAwAHNz2GPAi2PFiK/6se05gogRZ8ijRaOHRU5DT0RFUkSHlkxhdmM2MS4zLjEwMCBsaW",
  "J2b3JiaXNnyKFFo4hEVVJBVElPTkSHkzAwOjAwOjAwLjMzMjAwMDAwMAAfQ7Z1QKTngQCjhYIAAIAAo6SBACCAMAIAnQEq",
  "IAAgAABHCIWFiIWEiAICAAeQ88nA/v+rUICjhYIAIIAAo4WCAECAAKOFggBggACjhYIAgIAAo5WBAIQAsQEAARAQABgAGF",
  "gv9AAIcACjhYIAoIAAo4WCAMCAAKOFggDggACjlYEA6ACxAQABEBAAGAAYWC/0AAhwAKOFggEAgACjhYIBIIAAo4WCAUCA",
  "ABxTu2uRu4+zgSC3iveBAfGCC0XwgQo=",
].join("");

test.beforeEach(async () => {
  await resetDatabase();
});

/** Uploads `base64` to the issue under `name` and `mimeType`, the route an upload from the
 *  Artifacts tab takes, and answers the artifact's slug with the address a drawn version loads
 *  from. */
async function uploadVersion(
  page: Page,
  issueKey: string,
  name: string,
  mimeType: string,
  base64: string
): Promise<{ address: string; slug: string }> {
  const answer = await page.request.post(`/api/v1/issues/${issueKey}/artifacts`, {
    headers: { Origin: dashboardOrigin },
    multipart: { file: { buffer: Buffer.from(base64, "base64"), mimeType, name }, name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  const { artifact } = (await answer.json()) as ArtifactUploadResponse;
  return {
    address: `/api/v1/issues/${issueKey}/artifacts/${artifact.slug}/versions/1`,
    slug: artifact.slug,
  };
}

/** Sends the browser to `address` from the page it is on, as a reader who opened the version's
 *  own link does, and fails unless the browser went there and handed the reader nothing to save.
 *  `page.goto` is not used: a browser that downloads an address instead of drawing it fails the
 *  navigation itself, and which of the two the browser did is this file's whole claim, so it is
 *  read as an assertion rather than as a thrown navigation. */
async function sendTo(page: Page, address: string, downloads: readonly string[]): Promise<void> {
  await page.evaluate((target) => {
    window.location.assign(target);
  }, address);
  await expect
    .poll(() => ({ downloaded: [...downloads], path: new URL(page.url()).pathname }))
    .toEqual({ downloaded: [], path: address });
}

test("a picture's own address draws the picture and hands the reader no download", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A picture's own address" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const downloads: string[] = [];
    page.on("download", (download) => downloads.push(download.suggestedFilename()));
    const picture = await uploadVersion(page, issue.key, "panel.png", "image/png", TINY_PNG);

    await page.goto(`/issues/${issue.key}`);
    await sendTo(page, picture.address, downloads);

    // The browser's own picture document: one `<img>`, holding the bytes it decoded.
    const drawn = page.locator("img");
    await expect(drawn).toHaveCount(1);
    await expect
      .poll(() => drawn.evaluate((image: HTMLImageElement) => image.naturalWidth))
      .toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("a video's own address plays the video and hands the reader no download", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A video's own address" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const downloads: string[] = [];
    page.on("download", (download) => downloads.push(download.suggestedFilename()));
    const video = await uploadVersion(page, issue.key, "clip.webm", "video/webm", TINY_WEBM);

    await page.goto(`/issues/${issue.key}`);
    await sendTo(page, video.address, downloads);

    // The browser's own video document: one player, which read the file's duration off the
    // bytes it was served. Both readings go through the page rather than through a locator.
    // Every locator call first waits out a navigation the frame still has in flight
    // (`Page._performWaitForNavigationCheck`, playwright-core 1.64.1), and WebKit's media
    // document leaves one in flight for good: the player it builds holds the document's own
    // address open, so the navigation never finishes and the locator never runs - it answered
    // `undefined` rather than a count. `page.evaluate` runs that check for no one and reads
    // the document the navigation committed, which is the document this row is about. A
    // picture document finishes its navigation, so the rows above read it with locators.
    await expect
      .poll(() => page.evaluate(() => document.querySelectorAll("video").length), {
        message: "the browser drew no player at the video's own address",
      })
      .toBe(1);
    await expect
      .poll(() => page.evaluate(() => document.querySelector("video")?.duration ?? 0), {
        message: "the player read no duration off the bytes it was served",
      })
      .toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("the artifact page's Download control saves the version under the artifact's name", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Downloading a picture" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const picture = await uploadVersion(page, issue.key, "panel.png", "image/png", TINY_PNG);

    await page.goto(`/issues/${issue.key}/artifacts/${picture.slug}?v=1`);
    const control = page.getByRole("link", { name: "Download version 1" }).first();
    await expect(control).toHaveAttribute("download", "");
    const [download] = await Promise.all([page.waitForEvent("download"), control.click()]);
    // The address ends in the version number; the saved file is named for the artifact.
    expect(download.suggestedFilename()).toBe("panel.png");
  } finally {
    await alice.close();
  }
});
