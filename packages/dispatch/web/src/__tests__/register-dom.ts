/**
 * happy-dom's DOM, global before anything that reads it is evaluated.
 *
 * This is the preload's first import, and it stays first: a module that reads a DOM global while
 * it is evaluated keeps whatever it found then, and several do.
 * `@testing-library/dom` builds `screen` from `document` as it is evaluated, and every query of it
 * throws "a global document has to be available" when there was none;
 * `@mswjs/interceptors` builds its requests with the `Request` bound when it is evaluated, and the
 * runtime's own refuses happy-dom's `AbortSignal` ("signal is not of type AbortSignal").
 * A static import is hoisted above the body of the file that writes it, so the registration cannot
 * live in that body - it lives here, in the module imported first. A bare side-effect import is
 * also the one import Biome's sorter will not move.
 */

import { GlobalRegistrator } from "@happy-dom/global-registrator";

/**
 * The origin the page is registered at, and the base every relative request resolves against. A
 * reserved TLD (RFC 6761), so a request that escaped MSW resolves nowhere rather than reaching a
 * host that answers. happy-dom's own default is `about:blank`, which has no origin, and a relative
 * URL does not resolve against it.
 */
export const TEST_ORIGIN = "http://dispatch.test";

GlobalRegistrator.register({ url: `${TEST_ORIGIN}/` });
