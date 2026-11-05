import { borderDefault } from "../../theme/classes";

/**
 * One class declaration per size an artifact's version is ever drawn at, read by both drawing
 * paths: the React unit (`ArtifactMedia.tsx`), which every page and body draws through, and the
 * document editors' node view (`features/doc/picture-view.ts`), which is plain DOM and can
 * mount no React component. They are declared here rather than beside either, because
 * `picture-viewer.ts` tells the two sizes apart by the thumbnail's class and the unit opens the
 * viewer through it.
 */

/** A picture at its own size, scaled down to the column it sits in and never capped in height,
 *  so a wide figure fills the column; a click opens it at full size (`openPictureViewer`).
 *  `not-prose` keeps Markdown's figure margins off it, so it sits in its line as a pasted picture
 *  is written. A video's player is drawn with the same string, so a picture in a spec, a picture
 *  in a comment and a player in either are one shape. */
export const pictureClassName = `not-prose inline-block h-auto w-auto max-w-full cursor-zoom-in rounded border object-contain align-bottom ${borderDefault}`;

/** The 40 px square a picture shows as beside its title, wherever a line holds it: a reference
 *  link's, an ask option's, a one-line preview's, the Artifacts tab's row. `not-prose` keeps
 *  Markdown's figure margins off it. A drawn thumbnail wears this string and nothing else, which
 *  is how `picture-viewer.ts` keeps thumbnails out of the set a reader steps through. */
export const pictureThumbnailClassName = `not-prose inline-block h-10 w-10 shrink-0 rounded border object-cover align-middle ${borderDefault}`;
