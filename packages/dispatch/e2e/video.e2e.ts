import { expect, type Locator, type Page, test } from "@playwright/test";

import type { ArtifactUploadResponse } from "../web/src/api/types";
import { agentRow, setLiveSessions } from "./agents";
import {
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  dashboardOrigin,
  editArtifact,
  getArtifact,
  getIssueArtifactText,
} from "./api";
import { documentEditor, openSpec, typeAtEnd } from "./editor";
import { resetDatabase } from "./seed";
import { sessionIds } from "./session-ids";
import { asUser } from "./users";

// A reference to an uploaded video draws a player where the same reference to a picture draws a
// picture: the markdown picture line everywhere, and a plain reference standing alone in a block
// of a rendered body. In the two document editors a plain reference is the link it has always
// been, and a reference inside a sentence is a link everywhere. The player loads from the
// version's own bytes route, as a picture does, and the document's markdown is the markdown its
// author wrote.
//
// The surfaces here are the three mappings that resolve a reference, so each is driven once: an
// issue's artifact through the issue summary (the spec editor, a past version, a comment and the
// artifact's own page), a project's through the document summary (a project document), and a
// conversation's through `agentReferenceArtifact` (a turn on the Agents page).
//
// Every helper here is this file's own: the file is run against a build of the change's merge
// base to see its rows fail, where a helper this change added would not load at all.

/** A 2x2 PNG: the negative control every row below carries, since the same code draws it and
 *  must draw no player for it. */
const TINY_PNG =
  "iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFUlEQVR4nGP8z4AATAwMDAwMDAwMAA3+AR6vmgTYAAAAAElFTkSuQmCC";

/** A 32x32 WebM of a third of a second, VP8 video and Vorbis audio, which Chromium decodes: a
 *  player drawn from it reports a duration of its own. */
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

/** A 32x32 MP4 of three tenths of a second, H.264 video and AAC audio. Which engines carry those
 *  codecs is the build's own business, so the MP4 rows read the player, its address and the
 *  answer the bytes route gives, never a decoded duration. */
const TINY_MP4 = [
  "AAAAIGZ0eXBpc29tAAACAGlzb21pc28yYXZjMW1wNDEAAAAIZnJlZQAAAxVtZGF0AAACuwYF//+33EXpvebZSLeWLNgg2S",
  "Pu73gyNjQgLSBjb3JlIDE2NCByMzE5MSA0NjEzYWMzIC0gSC4yNjQvTVBFRy00IEFWQyBjb2RlYyAtIENvcHlsZWZ0IDIw",
  "MDMtMjAyNCAtIGh0dHA6Ly93d3cudmlkZW9sYW4ub3JnL3gyNjQuaHRtbCAtIG9wdGlvbnM6IGNhYmFjPTEgcmVmPTMgZG",
  "VibG9jaz0xOjA6MCBhbmFseXNlPTB4MzoweDExMyBtZT1oZXggc3VibWU9NyBwc3k9MSBwc3lfcmQ9MS4wMDowLjAwIG1p",
  "eGVkX3JlZj0xIG1lX3JhbmdlPTE2IGNocm9tYV9tZT0xIHRyZWxsaXM9MSA4eDhkY3Q9MSBjcW09MCBkZWFkem9uZT0yMS",
  "wxMSBmYXN0X3Bza2lwPTEgY2hyb21hX3FwX29mZnNldD0tMiB0aHJlYWRzPTEgbG9va2FoZWFkX3RocmVhZHM9MSBzbGlj",
  "ZWRfdGhyZWFkcz0wIG5yPTAgZGVjaW1hdGU9MSBpbnRlcmxhY2VkPTAgYmx1cmF5X2NvbXBhdD0wIGNvbnN0cmFpbmVkX2",
  "ludHJhPTAgYmZyYW1lcz0zIGJfcHlyYW1pZD0yIGJfYWRhcHQ9MSBiX2JpYXM9MCBkaXJlY3Q9MSB3ZWlnaHRiPTEgb3Bl",
  "bl9nb3A9MCB3ZWlnaHRwPTIga2V5aW50PTI1MCBrZXlpbnRfbWluPTEwIHNjZW5lY3V0PTQwIGludHJhX3JlZnJlc2g9MC",
  "ByY19sb29rYWhlYWQ9NDAgcmM9YWJyIG1idHJlZT0xIGJpdHJhdGU9OCByYXRldG9sPTEuMCBxY29tcD0wLjYwIHFwbWlu",
  "PTAgcXBtYXg9NjkgcXBzdGVwPTQgaXBfcmF0aW89MS40MCBhcT0xOjEuMDAAgAAAABNliIQAI//+8ivwKZ2BxsD3nD+B3A",
  "BMYXZjNjEuMy4xMDAAAjBADgAAAAhBmiJsR//+GAAAAAgBnkF5H/+DgQEYIAcBGCAHARggBwAABcRtb292AAAAbG12aGQA",
  "AAAAAAAAAAAAAAAAAAPoAAABLAABAAABAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAA",
  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADAAAClnRyYWsAAABcdGtoZAAAAAMAAAAAAAAAAAAAAAEAAAAAAAABLAAA",
  "AAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAEAAAAAAAAAAAAAAAAAAEAAAAAAIAAAACAAAAAAACRlZHRzAAAAHG",
  "Vsc3QAAAAAAAAAAQAAASwAAAgAAAEAAAAAAg5tZGlhAAAAIG1kaGQAAAAAAAAAAAAAAAAAACgAAAAMAFXEAAAAAAAtaGRs",
  "cgAAAAAAAAAAdmlkZQAAAAAAAAAAAAAAAFZpZGVvSGFuZGxlcgAAAAG5bWluZgAAABR2bWhkAAAAAQAAAAAAAAAAAAAAJG",
  "RpbmYAAAAcZHJlZgAAAAAAAAABAAAADHVybCAAAAABAAABeXN0YmwAAAC9c3RzZAAAAAAAAAABAAAArWF2YzEAAAAAAAAA",
  "AQAAAAAAAAAAAAAAAAAAAAAAIAAgAEgAAABIAAAAAAAAAAEUTGF2YzYxLjMuMTAwIGxpYngyNjQAAAAAAAAAAAAAAAAY//",
  "8AAAAzYXZjQwFkAAr/4QAXZ2QACqzZSWwEQAAAAwBAAAAFA8SJZYABAAVo6+yyLP34+AAAAAAQcGFzcAAAAAEAAAABAAAA",
  "FGJ0cnQAAAAAAABOIAAATiAAAAAYc3R0cwAAAAAAAAABAAAAAwAABAAAAAAUc3RzcwAAAAAAAAABAAAAAQAAAChjdHRzAA",
  "AAAAAAAAMAAAABAAAIAAAAAAEAAAwAAAAAAQAABAAAAAAoc3RzYwAAAAAAAAACAAAAAQAAAAEAAAABAAAAAgAAAAIAAAAB",
  "AAAAIHN0c3oAAAAAAAAAAAAAAAMAAALWAAAADAAAAAwAAAAYc3RjbwAAAAAAAAACAAAAMAAAAxkAAAJZdHJhawAAAFx0a2",
  "hkAAAAAwAAAAAAAAAAAAAAAgAAAAAAAAEsAAAAAAAAAAAAAAABAQAAAAABAAAAAAAAAAAAAAAAAAAAAQAAAAAAAAAAAAAA",
  "AAAAQAAAAAAAAAAAAAAAAAAAJGVkdHMAAAAcZWxzdAAAAAAAAAABAAABLAAABAAAAQAAAAAB0W1kaWEAAAAgbWRoZAAAAA",
  "AAAAAAAAAAAAAAH0AAAA1gVcQAAAAAAC1oZGxyAAAAAAAAAABzb3VuAAAAAAAAAAAAAAAAU291bmRIYW5kbGVyAAAAAXxt",
  "aW5mAAAAEHNtaGQAAAAAAAAAAAAAACRkaW5mAAAAHGRyZWYAAAAAAAAAAQAAAAx1cmwgAAAAAQAAAUBzdGJsAAAAfnN0c2",
  "QAAAAAAAAAAQAAAG5tcDRhAAAAAAAAAAEAAAAAAAAAAAABABAAAAAAH0AAAAAAADZlc2RzAAAAAAOAgIAlAAIABICAgBdA",
  "FQAAAAAAH0AAAAJDBYCAgAUViFblAAaAgIABAgAAABRidHJ0AAAAAAAAH0AAAAJDAAAAIHN0dHMAAAAAAAAAAgAAAAMAAA",
  "QAAAAAAQAAAWAAAAAoc3RzYwAAAAAAAAACAAAAAQAAAAEAAAABAAAAAgAAAAMAAAABAAAAJHN0c3oAAAAAAAAAAAAAAAQA",
  "AAATAAAABAAAAAQAAAAEAAAAGHN0Y28AAAAAAAAAAgAAAwYAAAMxAAAAGnNncGQBAAAAcm9sbAAAAAIAAAAB//8AAAAcc2",
  "JncAAAAAByb2xsAAAAAQAAAAQAAAABAAAAYXVkdGEAAABZbWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAbWRpcmFwcGwAAAAA",
  "AAAAAAAAAAAsaWxzdAAAACSpdG9vAAAAHGRhdGEAAAABAAAAAExhdmY2MS4xLjEwMA==",
].join("");

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-video" },
  as: "agent" as const,
};

test.beforeEach(async () => {
  await resetDatabase();
});

/** Uploads `base64` under `name` and `mimeType` to an artifact owner's own upload route - an
 *  issue's, a project's or a session's - and answers the artifact's slug. The stored type is the
 *  one declared here: Dispatch sniffs a picture out of the bytes and takes every other upload at
 *  the uploader's word, which is how a video is stored under a video type. */
async function uploadVersion(
  page: Page,
  artifactsPath: string,
  name: string,
  mimeType: string,
  base64: string
): Promise<string> {
  const answer = await page.request.post(artifactsPath, {
    headers: { Origin: dashboardOrigin },
    multipart: { file: { buffer: Buffer.from(base64, "base64"), mimeType, name }, name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  const { artifact } = (await answer.json()) as ArtifactUploadResponse;
  return artifact.slug;
}

/** Fails unless `player` is the `<video controls preload="metadata">` drawn from `src`. */
async function expectPlayer(player: Locator, src: string): Promise<void> {
  await expect(player).toHaveCount(1);
  await expect(player).toBeVisible();
  await expect(player).toHaveAttribute("src", src);
  await expect(player).toHaveAttribute("preload", "metadata");
  expect(await player.evaluate((element: HTMLVideoElement) => element.controls)).toBe(true);
}

/** Fails unless the browser read a duration off the bytes behind `player`. Only the WebM fixture
 *  is held to this: which other containers an engine decodes is the build's own business. */
async function expectPlays(player: Locator): Promise<void> {
  await player.scrollIntoViewIfNeeded();
  await expect
    .poll(() => player.evaluate((element: HTMLVideoElement) => element.duration))
    .toBeGreaterThan(0);
}

/** Fails unless `picture` has loaded real pixels: scrolled to first, since it loads lazily. */
async function expectDrawn(picture: Locator, src: string): Promise<void> {
  await expect(picture).toHaveAttribute("src", src);
  await picture.scrollIntoViewIfNeeded();
  await expect
    .poll(() => picture.evaluate((image: HTMLImageElement) => image.naturalWidth))
    .toBeGreaterThan(0);
}

/** The server's answer to the next `POST` to `path`, checked to be a success so a refusal fails
 *  naming its status rather than timing out. */
async function expectPosted(page: Page, path: string, act: () => Promise<void>): Promise<void> {
  const answer = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" && new URL(response.url()).pathname === path
  );
  await act();
  const response = await answer;
  expect(response.status(), `POST ${path}`).toBeLessThan(300);
}

/** Writes `markdown` at the end of the issue's spec as an agent's `doc-edit` does, and answers
 *  the version number the server gave that write. */
async function writeIntoSpec(artifactId: string, markdown: string): Promise<number> {
  await editArtifact(artifactId, { ops: [{ after: "end", markdown, op: "insert" }] }, agentSession);
  const newest = async () =>
    Math.max(...(await getArtifact(artifactId)).versions.map(({ number }) => number));
  await expect.poll(newest).toBeGreaterThan(1);
  return newest();
}

test("a video reference draws a player in the spec editor and in a past version, where a picture stays a picture and a link stays a link @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Clips\n\nIntro.",
    title: "A video in a spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/issues/${issue.key}/artifacts`;
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);
    const clipRef = `dispatch://${issue.key}/artifact/clip-webm@v1`;
    const panelRef = `dispatch://${issue.key}/artifact/panel-png@v1`;
    const clipSrc = `${artifacts}/clip-webm/versions/1`;
    const panelSrc = `${artifacts}/panel-png/versions/1`;
    const clipLine = `![clip.webm](${clipRef})`;
    const linkLine = `[the clip](${clipRef})`;
    const panelLine = `![panel.png](${panelRef})`;
    const pictured = await writeIntoSpec(
      issue.primary_artifact_id,
      `${clipLine}\n\n${linkLine}\n\n${panelLine}\n\nBelow the clip.`
    );

    await openSpec(page, issue.key, "Below the clip.");
    const editor = documentEditor(page);
    await expectPlayer(editor.locator(`[data-dispatch-doc-picture="${clipRef}"] video`), clipSrc);
    await expectPlays(editor.locator(`[data-dispatch-doc-picture="${clipRef}"] video`));
    // The negative control: the same node view, the same reference grammar, a picture.
    await expectDrawn(editor.locator(`[data-dispatch-doc-picture="${panelRef}"] img`), panelSrc);
    await expect(editor.locator(`[data-dispatch-doc-picture="${panelRef}"] video`)).toHaveCount(0);
    // A plain reference in an editor is the link it has always been for a picture, and the one
    // player on the page is the picture line's.
    await expect(editor.getByRole("link", { name: "the clip" })).toBeVisible();
    await expect(editor.locator("video")).toHaveCount(1);

    // Editing around the player leaves the author's markdown as it was written.
    await typeAtEnd(page, "Typed in the editor.");
    const markdown = async () => (await getIssueArtifactText(issue.key, "spec")).markdown;
    await expect.poll(markdown).toContain("Typed in the editor.");
    const saved = await markdown();
    expect(saved).toContain(clipLine);
    expect(saved).toContain(linkLine);
    expect(saved).toContain(panelLine);

    // The past version draws the same three things through the read-only view.
    await page.getByRole("combobox", { name: "Version" }).selectOption(String(pictured));
    const version = page.getByTestId("version-view");
    await expectPlayer(version.locator(`[data-dispatch-doc-picture="${clipRef}"] video`), clipSrc);
    await expectPlays(version.locator(`[data-dispatch-doc-picture="${clipRef}"] video`));
    await expectDrawn(version.locator(`[data-dispatch-doc-picture="${panelRef}"] img`), panelSrc);
    await expect(version.getByRole("link", { name: "the clip" })).toBeVisible();
    await expect(version.locator("video")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a video artifact's own page plays it where a picture's page shows the picture @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A video's own page" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/issues/${issue.key}/artifacts`;
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);

    await page.goto(`/issues/${issue.key}/artifacts/clip-webm?v=1`);
    const shown = page.getByRole("region", { name: "clip.webm version 1" });
    await expectPlayer(shown.locator("video"), `${artifacts}/clip-webm/versions/1`);
    await expectPlays(shown.locator("video"));

    // The negative control: the picture's page draws a picture and no player.
    await page.goto(`/issues/${issue.key}/artifacts/panel-png?v=1`);
    const picture = page.getByRole("region", { name: "panel.png version 1" });
    await expectDrawn(picture.locator("img"), `${artifacts}/panel-png/versions/1`);
    await expect(picture.locator("video")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

test("a video reference in a comment draws a player for the picture line and for a reference alone in its block, and stays a link inside a sentence", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A video in a comment" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/issues/${issue.key}/artifacts`;
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);
    const clipRef = `dispatch://${issue.key}/artifact/clip-webm@v1`;
    const panelRef = `dispatch://${issue.key}/artifact/panel-png@v1`;
    const clipSrc = `${artifacts}/clip-webm/versions/1`;
    const comment = await createComment(issue.key, {
      body: [
        `![clip.webm](${clipRef})`,
        clipRef,
        `Watch ${clipRef} before deciding.`,
        `![panel.png](${panelRef})`,
      ].join("\n\n"),
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    const card = page.locator(`[data-turn="comment:${comment.id}"]`);
    // A collapsed card is a one-line preview, where no reference is at reading size; the card
    // the reader opened is the body.
    await card.getByRole("button", { name: "Expand thread" }).click();
    await expect(card.locator("article")).toHaveAttribute("aria-expanded", "true");

    await expectPlayer(card.locator(`[data-dispatch-picture="${clipRef}"] video`), clipSrc);
    await expectPlays(card.locator(`[data-dispatch-picture="${clipRef}"] video`));

    // Two plain references, in the body's order: the one standing alone in its block, whose
    // anchor gives up its address for as long as the player is inside it, then the one inside a
    // sentence, which is the link it has always been.
    const references = card.locator(`a[data-dispatch-ref="${clipRef}"]`);
    await expect(references).toHaveCount(2);
    await expectPlayer(references.nth(0).locator("video"), clipSrc);
    await expect(references.nth(0)).not.toHaveAttribute("href", /./);
    await expect(references.nth(1)).toHaveAttribute(
      "href",
      `/issues/${issue.key}/artifacts/clip-webm?v=1`
    );
    await expect(references.nth(1).locator("video")).toHaveCount(0);

    // The negative control: the same body's picture line is a picture, with no player anywhere
    // but the two the video earned.
    await expectDrawn(
      card.locator(`[data-dispatch-picture="${panelRef}"] img`),
      `${artifacts}/panel-png/versions/1`
    );
    await expect(card.locator("video")).toHaveCount(2);
  } finally {
    await alice.close();
  }
});

test("a video sent in a direct message draws a player in the conversation on the Agents page", async ({
  browser,
}) => {
  const planner = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    machine_id: "planner-host",
    roles: ["planner"],
    session_id: sessionIds.planner,
    title: "Planner",
  };
  await setLiveSessions([planner]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/agents/${planner.session_id}/artifacts`;
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);
    const clipRef = `dispatch://agent/${planner.session_id}/artifact/clip-webm@v1`;
    const panelRef = `dispatch://agent/${planner.session_id}/artifact/panel-png@v1`;

    await page.goto("/agents");
    await page
      .getByRole("region", { name: "Agents" })
      .getByRole("button", { name: "No Dispatch activity (1)" })
      .click();
    const row = agentRow(page, planner.session_id);
    await row.getByRole("button", { exact: true, name: "Planner" }).click();
    const field = row.getByRole("textbox", { name: "Comment" });
    // A video dropped into a direct message is written as its plain reference, on a line of its
    // own, and a picture as the markdown line (`uploadedFileText`), so the message holds one of
    // each exactly as a sender's own paste leaves it.
    await field.fill(`${clipRef}\n\n![panel.png](${panelRef})`);
    await expectPosted(page, `/api/v1/agents/${planner.session_id}/messages`, () =>
      field.press("Control+Enter")
    );

    const conversation = row.getByRole("list", { name: "Conversation with Planner" });
    const reference = conversation.locator(`a[data-dispatch-ref="${clipRef}"]`);
    await expectPlayer(reference.locator("video"), `${artifacts}/clip-webm/versions/1`);
    await expectPlays(reference.locator("video"));
    // The anchor gives up its address for as long as the player is inside it, so a press on the
    // player's controls plays instead of opening the artifact's page.
    await expect(reference).not.toHaveAttribute("href", /./);
    // The negative control: the picture beside it is a picture.
    await expectDrawn(
      conversation.locator(`[data-dispatch-picture="${panelRef}"] img`),
      `${artifacts}/panel-png/versions/1`
    );
    await expect(conversation.locator("video")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("a video reference in a project document draws a player in its editor", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = "/api/v1/projects/CORE/artifacts";
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);
    const clipRef = "dispatch://CORE/artifact/clip-webm@v1";
    const panelRef = "dispatch://CORE/artifact/panel-png@v1";
    const notes = await createProjectDocument("CORE", {
      content: `# Notes\n\n![clip.webm](${clipRef})\n\n![panel.png](${panelRef})\n\nBelow the clip.\n`,
      name: "notes.md",
    });

    await page.goto(`/projects/CORE/documents/${notes.artifact.slug}`);
    const editor = documentEditor(page);
    await expect(editor).toContainText("Below the clip.");
    await expectPlayer(
      editor.locator(`[data-dispatch-doc-picture="${clipRef}"] video`),
      `${artifacts}/clip-webm/versions/1`
    );
    await expectPlays(editor.locator(`[data-dispatch-doc-picture="${clipRef}"] video`));
    // The negative control: the picture beside it is a picture.
    await expectDrawn(
      editor.locator(`[data-dispatch-doc-picture="${panelRef}"] img`),
      `${artifacts}/panel-png/versions/1`
    );
    await expect(editor.locator("video")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

test("an MP4 reference draws a player from a bytes route that answers it inline, by name and in parts", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Clips\n\nIntro.",
    title: "An MP4 in a spec",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/issues/${issue.key}/artifacts`;
    await uploadVersion(page, artifacts, "reel.mp4", "video/mp4", TINY_MP4);
    const reelRef = `dispatch://${issue.key}/artifact/reel-mp4@v1`;
    const reelSrc = `${artifacts}/reel-mp4/versions/1`;
    await writeIntoSpec(issue.primary_artifact_id, `![reel.mp4](${reelRef})\n\nBelow the reel.`);

    await openSpec(page, issue.key, "Below the reel.");
    await expectPlayer(
      documentEditor(page).locator(`[data-dispatch-doc-picture="${reelRef}"] video`),
      reelSrc
    );

    // What the player asks for, and what the route answers it: the stored type, the file's own
    // name shown where the reader is, a quoted entity tag, and parts of the file on request.
    const whole = await page.request.get(reelSrc);
    expect(whole.status()).toBe(200);
    expect(whole.headers()["content-type"]).toBe("video/mp4");
    expect(whole.headers()["content-disposition"]).toBe(
      `inline; filename="reel.mp4"; filename*=UTF-8''reel.mp4`
    );
    expect(whole.headers()["accept-ranges"]).toBe("bytes");
    expect(whole.headers().etag).toMatch(/^"[0-9a-f]{64}"$/);

    const part = await page.request.get(reelSrc, { headers: { Range: "bytes=0-15" } });
    expect(part.status()).toBe(206);
    expect(part.headers()["content-range"]).toMatch(/^bytes 0-15\/\d+$/);
    expect((await part.body()).length).toBe(16);
  } finally {
    await alice.close();
  }
});

test("a video named by the picture line in a turn whose summary is unread plays, and leaves no dead link", async ({
  browser,
}) => {
  const planner = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    machine_id: "planner-host",
    roles: ["planner"],
    session_id: sessionIds.planner,
    title: "Planner",
  };
  await setLiveSessions([planner]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const artifacts = `/api/v1/agents/${planner.session_id}/artifacts`;
    await uploadVersion(page, artifacts, "clip.webm", "video/webm", TINY_WEBM);
    await uploadVersion(page, artifacts, "panel.png", "image/png", TINY_PNG);
    const clipRef = `dispatch://agent/${planner.session_id}/artifact/clip-webm@v1`;
    const panelRef = `dispatch://agent/${planner.session_id}/artifact/panel-png@v1`;

    await page.goto("/agents");
    await page
      .getByRole("region", { name: "Agents" })
      .getByRole("button", { name: "No Dispatch activity (1)" })
      .click();
    const row = agentRow(page, planner.session_id);
    await row.getByRole("button", { exact: true, name: "Planner" }).click();
    const field = row.getByRole("textbox", { name: "Comment" });
    // The pinned picture line, which is the line `dispatch artifact` prints for an upload and a
    // paste writes. Nothing on this page has read the artifact's summary when the turn is
    // drawn, so the line draws from the address it names before anything knows the address is a
    // video's: the browser refuses those bytes as a picture, and the player must still arrive
    // when the summary lands, at that same address.
    await field.fill(`![clip.webm](${clipRef})\n\n![panel.png](${panelRef})`);
    await expectPosted(page, `/api/v1/agents/${planner.session_id}/messages`, () =>
      field.press("Control+Enter")
    );

    const conversation = row.getByRole("list", { name: "Conversation with Planner" });
    const line = conversation.locator(`[data-dispatch-picture="${clipRef}"]`);
    await expectPlayer(line.locator("video"), `${artifacts}/clip-webm/versions/1`);
    await expectPlays(line.locator("video"));
    // The reader is left with no plain link where the refused picture was, and no element the
    // browser never drew.
    await expect(line.locator("img")).toHaveCount(0);
    // The negative control: the picture line beside it is a picture.
    await expectDrawn(
      conversation.locator(`[data-dispatch-picture="${panelRef}"] img`),
      `${artifacts}/panel-png/versions/1`
    );
    await expect(conversation.locator("video")).toHaveCount(1);
  } finally {
    await alice.close();
  }
});
