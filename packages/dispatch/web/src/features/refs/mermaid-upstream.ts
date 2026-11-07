/**
 * Upstream `mermaid`, the second renderer of Dispatch's diagram Web Worker: it draws a `timeline`,
 * a `mindmap`, a `pie` and every other type beautiful-mermaid has no layout for, beside the six
 * families beautiful-mermaid draws (`mermaid-render.worker.ts` routes a source to one of the two).
 *
 * Nothing imports a value of this module statically: the worker reaches it with
 * `await import(...)` and takes only a type from it otherwise, which is erased, so a reader
 * whose page holds only the families beautiful-mermaid draws never fetches mermaid, d3,
 * cytoscape or this module's chunk -- the DOM shim, which this module owns, among it.
 *
 * mermaid and d3 read `document` while their own modules evaluate, so `installDomShim` runs
 * before the import rather than before the first render.
 *
 * Every colour below is a literal, measured against the two page backgrounds Dispatch draws on
 * (`#f8fafc` light, `#020618` dark), except the seven keys mermaid accepts a CSS custom property
 * in: those read `--mermaid-*` off the page (`web/src/styles.css`), which is how a label and a
 * border that sit on the page follow the reader's colour scheme. mermaid throws
 * `Unsupported color format: "var(--x)"` for a custom property in any key it derives a colour
 * from or fades, which is every other one.
 */
import { installDomShim, LAYOUT_WIDTH } from "./mermaid-dom-shim";

/** The longest source Mermaid is asked to draw. */
export const MERMAID_MAX_SOURCE_CHARS = 50_000;

/** Draws one source to SVG markup, still unsanitized. */
export type MermaidRenderer = (source: string) => Promise<string>;

/** The label a mark carries: `slate-50`, the lightest swatch of Dispatch's palette. Every mark it
 *  labels -- a colour slot's `MARK_FILLS` entry, `primaryColor` and the two beside it, a note --
 *  is a literal that is the same in both colour schemes, so this one value reads on all of them
 *  (4.55:1 at worst, 9.90:1 at best). The one mark it does not label is `MARK_FILL_LAST`. */
const LABEL_ON_MARK = "#f8fafc";

/** A line with nothing but the page behind it -- a flowchart's edges, the pie's rim, the
 *  timeline's axis: `slate-500`, the one swatch that reads against both page backgrounds
 *  (4.55:1 on `#f8fafc`, 4.23:1 on `#020618`), so it needs no second value per scheme. */
const LINE_ON_PAGE = "#62748e";

/** The fill of a mark, by colour slot. mermaid draws a slot's colour as both the fill of its mark
 *  (a timeline's event, a mindmap's node, a pie's slice) and the stroke of the edge that leaves
 *  it, with the page behind that edge, so a slot's luminance has to sit in the band that is dark
 *  enough for `LABEL_ON_MARK` to read on it (4.5:1) and light enough to read against the dark page
 *  (3:1). Dispatch's palette holds exactly five swatches in that band, one per hue; slot 5 and up
 *  repeat the cycle, because a sixth section on one diagram is rarer than an unreadable label. */
const MARK_FILLS = [
  // sky-700: label 5.60:1; against the page 5.60:1 light, 3.44:1 dark.
  "#0069a8",
  // emerald-700: label 5.13:1; against the page 5.13:1 light, 3.76:1 dark.
  "#007a55",
  // amber-700: label 4.81:1; against the page 4.81:1 light, 4.01:1 dark.
  "#bb4d00",
  // rose-700: label 5.76:1; against the page 5.76:1 light, 3.34:1 dark.
  "#c70036",
  // slate-500: label 4.55:1; against the page 4.55:1 light, 4.23:1 dark. The same swatch a line
  // on the page is drawn in, which is why the fifth slot's edges vanish into their own marks.
  LINE_ON_PAGE,
];

/** mermaid's colour slots, `cScale0` to `cScale11` (`THEME_COLOR_LIMIT`, which no theme variable
 *  below changes). */
const COLOR_SLOTS = 12;

/** The last slot's own fill: `white`. Every `.lineWrapper line` rule a timeline writes carries the
 *  last slot's label colour, and the last rule wins, so that one label colour draws the timeline's
 *  axis over the page and has to be `LINE_ON_PAGE` rather than `LABEL_ON_MARK`. The only other
 *  thing that reads it is a twelfth section's own label, so the twelfth slot's mark is filled
 *  white, where `LINE_ON_PAGE` reads at 4.76:1. */
const MARK_FILL_LAST = "#ffffff";

/** The mark mermaid derives every border and backdrop from: `slate-700`, the fill of a node, a
 *  state, an actor, a gantt task and a note. `LABEL_ON_MARK` reads on it at 9.90:1. It sits
 *  1.95:1 from the dark page, which is what `--mermaid-node-border` answers for: in the dark
 *  scheme that border is a lighter slate. */
const MARK_FILL_PRIMARY = "#314158";

/** The keys mermaid takes a CSS custom property in, bound to Dispatch's own properties: seven of
 *  its own and wardley's nested one. The page defines each property per colour scheme
 *  (`styles.css`), and a custom property inherits into the open shadow root the diagram is
 *  mounted in, which is how it reaches the drawing. */
const PAGE_PALETTE = {
  // Text with the page behind it: a diagram's labels outside any mark, the pie's title and
  // legend. `slate-900` light, `slate-100` dark, the page's own text colours.
  textColor: "var(--mermaid-text-color)",
  // The border of a node, drawn where its fill meets the page.
  nodeBorder: "var(--mermaid-node-border)",
  // A node's own label, which sits on that node's fill.
  nodeTextColor: "var(--mermaid-node-text-color)",
  // A diagram's title, drawn with nothing but the page behind it.
  titleColor: "var(--mermaid-title-color)",
  // The border of every mark mermaid derives from `primaryColor` (a state, a gantt task, a
  // requirement), drawn where that mark meets the page.
  primaryBorderColor: "var(--mermaid-primary-border-color)",
  // The page's own font, so a diagram's labels read as the body text around them.
  fontFamily: "var(--mermaid-font-family)",
  // 16px, which is also what the worker's shim falls back to while it measures a label (the
  // page's custom properties do not resolve in the worker), so the boxes mermaid lays out hold
  // the text the page paints.
  fontSize: "var(--mermaid-font-size)",
  // wardley paints its axis labels, its stage labels, its title and its notes with
  // `themeVariables.wardley?.axisTextColor ?? themeVariables.primaryTextColor`
  // (`wardleyDiagram-*.mjs`), and that fallback is `LABEL_ON_MARK`, the near-white a label on a
  // mark carries. These labels have the transparent canvas behind them rather than a mark, so
  // the fallback leaves them invisible on the light page; they are text on the page like any
  // other, and read through the same property as `textColor`. mermaid passes the value straight
  // to a `fill` attribute, deriving and fading nothing from it, so a custom property holds here.
  wardley: { axisTextColor: "var(--mermaid-text-color)" },
};

/** The colours that cannot follow the reader's scheme, and so are literals measured against both
 *  page backgrounds: the ones mermaid derives every other colour from, and the two a diagram
 *  fades. A mark is filled dark and labelled `LABEL_ON_MARK` in both schemes: `primaryColor` and
 *  the two beside it carry every border and backdrop mermaid derives, and a custom property
 *  throws there, so the pair that has to read is the mark's fill and its label, never the mark
 *  and the page. The border that does meet the page is `primaryBorderColor`, which does follow
 *  it. */
const MARK_PALETTE = {
  // The canvas stays transparent, as `.dispatch-mermaid` makes beautiful-mermaid's drawing: a
  // diagram sits on the reader's page rather than on a sheet of its own. Every backdrop mermaid
  // fills with this one -- a composite state's, an xychart's -- is transparent for the same
  // reason, and the text on it reads against the page through `textColor`.
  background: "transparent",
  // The mark every border and backdrop is derived from (`MARK_FILL_PRIMARY`).
  primaryColor: MARK_FILL_PRIMARY,
  primaryTextColor: LABEL_ON_MARK,
  // sky-800: the second mark (a sequence activation, a gantt alternating section). Label 7.18:1.
  secondaryColor: "#00598a",
  secondaryTextColor: LABEL_ON_MARK,
  // emerald-800: the third mark (an error backdrop, a journey's rectangle). Label 7.27:1.
  tertiaryColor: "#006045",
  tertiaryTextColor: LABEL_ON_MARK,
  // Every edge and arrowhead mermaid draws between marks, over the page.
  lineColor: LINE_ON_PAGE,
  arrowheadColor: LINE_ON_PAGE,
  // An edge's label carries no chip of its own, so it sits on the page and reads through
  // `textColor`, which follows the scheme; an opaque chip could not, being a literal here.
  edgeLabelBackground: "transparent",
  // A subgraph's backdrop, which sits on the page and carries its title: `slate-500` at a
  // quarter, the one literal that reads in both schemes through both of the ways a diagram uses
  // it. A flowchart fills the backdrop with this value as given, where the title reads 12.55:1
  // light and 14.46:1 dark; `block-beta` fades it to half (`fade(clusterBkg, 0.5)`), where the
  // title reads 8.79:1 and 9.94:1 and the backdrop stands 1.94:1 and 1.85:1 off the page. It
  // cannot be a custom property, because khroma's `fade` throws on one, and it cannot be
  // `transparent` either: khroma drops the alpha, so `fade` turns it into half-opaque black,
  // which leaves a block diagram's title at 4.32:1 on the light page.
  clusterBkg: `${LINE_ON_PAGE}40`,
  // That backdrop's border, drawn over the page, and faded the same way by `block-beta`
  // (`fade(clusterBorder, 0.2)`), so it is a literal for the same reason.
  clusterBorder: LINE_ON_PAGE,
  // A note is a mark like any other, and its text is the label on it.
  noteBkgColor: MARK_FILL_PRIMARY,
  noteTextColor: LABEL_ON_MARK,
  // The label on every mark drawn from a colour slot (`cScaleLabel0` and up read this), which is
  // a timeline's event, a mindmap's node and a journey's task.
  scaleLabelColor: LABEL_ON_MARK,
  // A gantt bar's text sits on the bar, which is `primaryColor`; the text outside a bar keeps
  // `textColor` and reads against the page.
  taskTextColor: LABEL_ON_MARK,
  taskTextLightColor: LABEL_ON_MARK,
  // A finished gantt bar is drawn in the line colour rather than mermaid's `lightgrey`, which no
  // scheme's text reads on.
  doneTaskBkgColor: LINE_ON_PAGE,
  doneTaskBorderColor: LINE_ON_PAGE,
  // The pie's slices are the colour slots again, and every stroke it draws -- the rim, and each
  // slice's own outline, whose outer arc has the page behind it -- is the line colour.
  pieStrokeColor: LINE_ON_PAGE,
  pieOuterStrokeColor: LINE_ON_PAGE,
  pieSectionTextColor: LABEL_ON_MARK,
  // Opaque slices. mermaid's own 0.7 would leave a slice's label sitting on the page as much as
  // on the slice, where no contrast between the two is defined.
  pieOpacity: "1",
  // A mindmap's root is filled from `git0` and labelled from `gitBranchLabel0`, not from a colour
  // slot, so the root carries the first slot's fill and the same label colour as every other node.
  git0: MARK_FILLS[0],
  gitBranchLabel0: LABEL_ON_MARK,
};

/** `cScale0`-`cScale11`, `cScaleInv0`-`cScaleInv11` and `pie1`-`pie12`: one entry per colour slot,
 *  which mermaid reads by name rather than from a list. */
function colorSlots(): Record<string, string> {
  const slots: Record<string, string> = {};
  for (let slot = 0; slot < COLOR_SLOTS; slot += 1) {
    const fill = MARK_FILLS[slot % MARK_FILLS.length];
    // The last slot is the one exception (see `MARK_FILL_LAST`), and only for `cScale`: the pie
    // labels every slice with one colour, so a white twelfth slice would carry an unreadable one.
    slots[`cScale${slot}`] = slot === COLOR_SLOTS - 1 ? MARK_FILL_LAST : fill;
    slots[`pie${slot + 1}`] = fill;
    // The line mermaid draws inside a mark of this slot (a timeline event's rule, a mindmap
    // node's) and under it, where the page shows through.
    slots[`cScaleInv${slot}`] = LINE_ON_PAGE;
  }
  // The last slot's label, which is also the timeline axis's stroke (see `MARK_FILL_LAST`).
  slots[`cScaleLabel${COLOR_SLOTS - 1}`] = LINE_ON_PAGE;
  return slots;
}

/** The ids handed to `mermaid.render`. Each call takes its own: the id names the drawing's own
 *  `<style>` and its markers, and the shim's document outlives every call. */
let nextRenderId = 0;

/** The one load, kept so a second source pays for neither the import nor `initialize` again. */
let loading: Promise<MermaidRenderer> | null = null;

/** The globals this worker's DOM shim defined, installing it where nothing has yet. This module
 *  is the one owner of that install: the worker reads the list from here, to take those globals
 *  away around a beautiful-mermaid call (`mermaid-render.worker.ts`, `withoutADom`), rather than
 *  reaching past this module into the shim it loads. */
export function mermaidShimGlobals(): readonly string[] {
  return installDomShim().globalsDefined;
}

async function load(): Promise<MermaidRenderer> {
  // Before the import, not before the first render: mermaid and d3 read `document` as their own
  // modules evaluate.
  mermaidShimGlobals();
  const { default: mermaid } = await import("mermaid");
  mermaid.initialize({
    // Nothing in a worker scans a page for diagrams; the renderer below is handed one source at a
    // time, and a scan of the shim's own document would draw whatever the last call left behind.
    startOnLoad: false,
    // Measured over 37 hostile sources in Chromium: strict removes every `<script>`, every `on*`
    // handler, every `javascript:` link, and `<use>` and `<iframe>` with it. A source cannot
    // raise its own level, because mermaid deletes every key of `secure` from a directive.
    securityLevel: "strict",
    // An HTML label is laid out by the browser, which a worker has none of, and it can carry a
    // remote `<img>` that fetches at render and again at mount. The nested key closes the
    // swimlane label, which reads `flowchart.htmlLabels` and emits an HTML label without it.
    htmlLabels: false,
    flowchart: { htmlLabels: false },
    // A refusal is the page's to draw (`mermaid-diagrams.ts` writes the status line); without
    // this mermaid leaves its own error drawing in the document and answers with it.
    suppressErrorRendering: true,
    // Without this every marker, clip path and gradient id inside a drawing is random, so one
    // source draws different markup on every call; counted ids make two drawings of a source
    // comparable, apart from the id this module hands each call.
    deterministicIds: true,
    // gantt sizes itself to its host element's `offsetWidth`, which no worker has, so it is laid
    // out at the width the shim's `screen` is (`LAYOUT_WIDTH`).
    gantt: { useWidth: LAYOUT_WIDTH },
    // What a source's own directive may not change. mermaid's default list protects `secure`
    // itself and the size limits; these four are the palette and the sanitizer's own level, and
    // a source-level `themeCSS` carrying `url(https://...)` would otherwise reach the drawing's
    // `<style>` and fetch at render and again at mount. Read from the installed package rather
    // than restated here, so a key mermaid adds to its own list stays protected.
    secure: [
      ...(mermaid.mermaidAPI.defaultConfig.secure ?? []),
      "themeCSS",
      "themeVariables",
      "fontFamily",
      "theme",
    ],
    // base is the one theme that takes its whole palette from `themeVariables`: it derives every
    // colour from the keys below, where `default` and the two dark themes mix in colours of their
    // own. Its `calculate` re-applies each given key after its derivations have run, so every
    // literal below reaches the drawing unchanged.
    theme: "base",
    themeVariables: { ...PAGE_PALETTE, ...MARK_PALETTE, ...colorSlots() },
  });
  return async (source: string) => {
    if (source.length > MERMAID_MAX_SOURCE_CHARS) {
      // Past this mermaid draws a diagram whose only text is `Maximum text size in diagram
      // exceeded` instead of throwing, and the reader would get that picture in place of a
      // reason. The message is what a reader reads after `Unable to render Mermaid diagram: `.
      throw new Error(
        `it is longer than ${MERMAID_MAX_SOURCE_CHARS.toLocaleString("en-US")} characters`
      );
    }
    const { svg } = await mermaid.render(`dispatch-mermaid-${nextRenderId++}`, source);
    return svg;
  };
}

/**
 * Loads the shim and Mermaid, configures Mermaid once, and answers the function that draws a
 * source to SVG markup.
 */
export async function loadMermaidRenderer(): Promise<MermaidRenderer> {
  loading ??= load();
  return loading;
}
