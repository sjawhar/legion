import { borderDefault } from "../../theme/classes";

/**
 * One class declaration per size an artifact's version is ever drawn at, read by both drawing
 * paths: the React unit (`ArtifactMedia.tsx`), which every page and body draws through, and the
 * document editors' node view (`features/doc/picture-view.ts`), which is plain DOM and can
 * mount no React component. They are declared here rather than beside either, because the two
 * paths draw one picture alike; each stamps the size it drew at on the element beside the class
 * (`data-dispatch-picture-size`), which is what `picture-viewer.ts` tells the sizes apart by.
 */

/** A picture at its own size, scaled down to the column it sits in and never capped in height,
 *  so a wide figure fills the column; a click opens it at full size (`openPictureViewer`).
 *  `not-prose` keeps Markdown's figure margins off it, so it sits in its line as a pasted picture
 *  is written. A video's player is drawn with the same string, so a picture in a spec, a picture
 *  in a comment and a player in either are one shape.
 *
 *  It declares no width of its own, only `max-w-full` and `h-auto`: a picture's `width` and
 *  `height` attributes, which every drawing place writes from the version's recorded size, are
 *  presentational hints, and any author `width` - `w-auto` as much as a number - wins over them
 *  in the cascade. With `w-auto` the element reserved nothing before its bytes arrived and the
 *  paragraph under an 800x600 picture sat at the top of the column; without it that paragraph
 *  sits 600 px down, where the loaded picture leaves it (INFRA-760 R12). */
export const pictureClassName = `not-prose inline-block h-auto max-w-full cursor-zoom-in rounded border object-contain align-bottom ${borderDefault}`;

/** The 40 px square a picture shows as beside its title, wherever a line holds it: a reference
 *  link's, an ask option's, a one-line preview's, the Artifacts tab's row. `not-prose` keeps
 *  Markdown's figure margins off it. What a drawn element was drawn at is stamped on it as
 *  `data-dispatch-picture-size`, not read back off this string, since a place may add a class
 *  of its own to it (`ArtifactMedia`). */
export const pictureThumbnailClassName = `not-prose inline-block h-10 w-10 shrink-0 rounded border object-cover align-middle ${borderDefault}`;
