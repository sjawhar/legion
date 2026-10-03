# Docs media

The scripts here make the site's screenshots and narrated videos from code, so each one can be
taken again when the product changes. Screenshots are not committed: CI
(`.github/workflows/docs.yaml`) takes them before every site build. Videos are committed with
everything needed to record them again.

| File | What it is |
| --- | --- |
| `harness.ts` | Boots Dispatch the way its e2e suite does (`packages/dispatch/e2e/run-server.sh` with the fake Envoy and fake GitHub, each browser signed in at the server's dev sign-in route), seeds example data, and checks a page is ready and clean before a capture. |
| `shot-runner.ts` | Takes a set of declared screenshots against the harness. |
| `shots.config.ts` | The Dispatch and Legion sets. |
| `shots.ts` | Takes every set: `shots.config.ts` and each `<section>/shots.config.ts`. |
| `narration.ts` | `NARRATION_VOICE`, the one voice every video is narrated in, and the call that speaks a section's text. |
| `recording.ts` | What a walkthrough file declares, and the helpers its browser sections act with: the drawn pointer, `pointTo`, `ring`, `highlight`, `scrollBy` and `linger`. |
| `walkthrough.ts` | Records, cuts, narrates and assembles one walkthrough into a video. |
| `walkthroughs/` | One file per video, holding each section's actions and narration; casts beside it in `walkthroughs/<name>/`. |

## Before you run anything

From the repository root:

```bash
bun install
(cd packages/dispatch && bun run build:web && bunx playwright install chromium)
```

The first command installs the workspace, the second builds the dashboard the harness serves and
the browser it is captured in. You also need Go, `psql`, and a Postgres database the run may
truncate, named by `DATABASE_URL`.
The harness listens on 8786, 9086 and 9087 (`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`,
`FAKE_GITHUB_PORT` move them) and refuses to start if one is taken. Every row it writes is example
data; nothing here reaches a real Dispatch, Envoy or GitHub.

## Screenshots

```bash
DATABASE_URL=postgres://<user>:<password>@<host>:<port>/<database> bun run docs:media
```

`bun run docs:media` runs `shots.ts`, which takes every shot. `--only inbox,board` takes the named
shots and `--set legion` one set. A set writes `docs/site/public/media/<set>/<id>.png`, and a page
embeds it with the site's base path:

```markdown
![The Dispatch Inbox: asks waiting on you, the blocking one first.](/legion/media/dispatch/inbox.png)
```

Use the shot's `alt` from the config, so the page and the image agree on what it shows.

### Adding a shot

Add an entry to a set's `shots`:

- `id`: the file name.
- `alt`: what the image shows, for the page that embeds it.
- `route`: the page to open, or a function of the seeded data.
- `viewport`: `desktop` (1440x900) or `phone` (iPhone 13, 390x844); `theme`: `light` or `dark`.
- `ready`: waits for the page's own readiness: the content the shot is of, visible. Wait on that
  content with `expect(...)`, never on a fixed sleep.
- `steps` (optional): clicks or keys that bring the page to the state the shot shows, ending on
  a wait for that state.
- `element` (optional): capture one element instead of the viewport.
- `prepare` (optional): API writes that move the seeded data on before the page opens. Shots run
  in the order declared, so the Legion set follows one issue from handover to sign-off.
- `allowEmpty` (optional): the `aria-label`s of empty states this shot may show.

Before every capture the runner waits for loading skeletons, images and fonts, then refuses a
screen showing an error (any visible `role="alert"`), the not-found page, an uncaught page
exception, or an empty state the shot does not allow. A refused shot fails the run, and what the
page showed is kept in `.work/failed-shots/`.

### A section's own set

A section that needs screens of its own adds `docs/site/media/<section>/shots.config.ts`, whose
default export is a `ShotSet` (or a list of them) from `shot-runner.ts`. Its `seed` resets nothing
itself: the runner resets the database before each set. Seed through the e2e suite's own helpers
with `dispatchApi()` and `fakeEnvoy()` from `harness.ts`, as `shots.config.ts` does. `shots.ts`
finds the file without being told.

## Narrated walkthroughs

A walkthrough is a file in `walkthroughs/` listing sections. Each section is recorded on its own,
so a weak one is re-recorded without touching the rest. Rebuild a video with:

```bash
DATABASE_URL=<database> ELEVENLABS_API_KEY=<key> bun docs/site/media/walkthrough.ts answer-an-ask
```

The walkthroughs so far are `answer-an-ask` and `broadcast-and-replies`. A build writes
`docs/site/public/media/videos/<name>.mp4`, captions in `<name>.vtt` from the narration, and a
poster frame in `<name>.jpg`. Commit all three with the walkthrough file. A page embeds them as:

```html
<video controls preload="metadata" poster="/legion/media/videos/answer-an-ask.jpg" src="/legion/media/videos/answer-an-ask.mp4">
  <track kind="captions" srclang="en" label="English" src="/legion/media/videos/answer-an-ask.vtt" default>
</video>
```

### What the pipeline enforces

- A browser section is recorded from the ready page to the end of its action, from the frames the
  browser draws (Playwright's screencast, JPEG at quality 100): each frame is shown from the moment
  it was drawn until the next, so a recording runs exactly as long as the wall clock did, and the
  report prints each section's action, recording length and frame count. Playwright's own video
  recorder is not used: its 1 Mbit/s VP8 blurs the page's text and borders at every keyframe.
- Video is never stretched, slowed or frozen to fit narration. Each narration line is generated
  after its clip is cut and measured, with ElevenLabs' leading and trailing silence removed, and
  placed at the moment it describes. The build fails when a line runs into the next one or past
  its clip: cut words, never footage pace. The lines are mixed onto silence the clip's length, so
  the audio runs unbroken from the clip's first frame: a gap in the audio's timestamps is one a
  browser plays straight through, and every later line would sound early.
- The build prints every silence of two seconds or more. Over live action (typing, a page
  updating) that is fine; over a still picture it is dead air, so shorten the hold.
- A section whose screen shows an error, at its start or its end, fails the recording: fix the
  cause and record it again.
- Every browser section runs in one 1024x896 frame (tall enough for the Inbox's top bar, heading
  and one whole ask card), at 25 fps, with the narration's loudness at -16 LUFS; the sections are
  then concatenated.

### The narration voice

Every video uses `NARRATION_VOICE` from `narration.ts`: ElevenLabs voice Daniel
(`onwK4e9ZLuTAKqWW03F9`), model `eleven_turbo_v2_5`, output `mp3_44100_192`, stability 0.75,
similarity boost 0.85, style 0, speed 0.92, speaker boost on. Import it rather than restating the
values. The key is read from `ELEVENLABS_API_KEY`. Narration is cached in `.work/` under its text
and voice, so a rebuild asks ElevenLabs only for words that changed.

### Adding a narrated section

Add a section to a walkthrough's `sections`:

- `id`: the section's name.
- `open(page, seeded)`: loads the page the section starts on and waits until it is ready. It is
  not recorded.
- `act(page, seeded, cue)`: the recorded action. Move with `pointTo(page, locator)` before each
  click, so the drawn pointer arrives before the click lands. `pointTo` refuses a target off
  screen: bring it into view with `scrollBy(page, pixels)`, which the viewer sees move, since an
  instant jump reads as a cut. Where a line names something on a still page, `highlight(page,
  locator)` moves the pointer there and rings it, since the pointer alone is too small a change
  to see. Use `linger(page, seconds)` where a viewer needs time to read; a linger is real page
  time, so keep it short. Call `cue(name)` at a moment a narration line describes, once that
  moment is on screen.
- `finish(page, seeded)` (optional): unrecorded, run after the action and before the page closes.
  A section that ends on a click (a send, a link) waits here for what the click starts, so its
  clip ends on the click and the next section opens on the loaded page rather than its loading
  skeleton.
- `narration`: the lines the narrator says, each `{ at, text }`, where `at` is seconds into the
  clip or the name of a cue. Write them after the clip is cut, to its measured length; each claims
  only what the screen shows, and is said after the action it names.

Then iterate on the footage without narrating it, and read the measured clip lengths it prints:

```bash
bun docs/site/media/walkthrough.ts <name> --only <section> --record-only
```

It prints each clip's length, with the action's and the recording's wall-clock seconds, and the
moment each cue landed. `--only` re-records the named sections and reuses the others' last
recordings from `.work/`; every section's action still runs, in order, so each finds the state the
ones before it left. The first build of a walkthrough records every section.

A video opens on its payoff within its first five seconds: show the result, then the recipe. Two
browser sections that meet on the same screen start the second with the pointer where the first
left it, so the cut changes nothing on screen.

### Terminal casts

A section can be an [asciinema](https://asciinema.org) recording instead of a browser action:

```ts
{ id: "state", cast: "legion-state/state.cast", window: [1.5, 14], narration: [{ at: 0.4, text: "…" }] }
```

Record each section on its own (`asciinema rec --cols 100 --rows 28 <file>.cast`) and commit the
`.cast` in `walkthroughs/<name>/`, beside its walkthrough file. The pipeline draws it with
[agg](https://github.com/asciinema/agg) (`cargo install --git https://github.com/asciinema/agg`;
the crate named `agg` is a different program), at the recording's own pace with no held last
frame, keeps `window` (seconds of the drawn recording, all of it when omitted), letterboxes it to
the video's frame and narrates it like any other section. An erroring command on screen is a
re-recording, not a trim. A walkthrough made only of casts needs no harness and no `seed`.

### Before you commit a video

Watch it, then read it. Make a contact sheet, a frame every 2.5 seconds
(`ffmpeg -i <video> -vf "fps=1/2.5,scale=640:-1,tile=3x5" -frames:v 1 sheet.png`), and again at
`scale=512:-1` for how it reads at half size; look densely at every section boundary. Crop small
text (sidebar labels, form fields, terminal chrome) at native size before reading it. Check that
no error shows anywhere on screen, that every caption line matches the frames it plays over, and
that nothing private is on screen.
