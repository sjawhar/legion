/**
 * Going is the answer Dispatch's own server writes: a question is answered from what Dispatch
 * holds, and the answer is streamed to the browser as server-sent events. Every name, code and
 * bound of that stream is declared here once, and generated into Go (`contracts.Going*`), so the
 * server that writes a frame and the page that reads it cannot drift apart.
 */

/** The frame carrying the next piece of the answer's text, in the order the model wrote it.
 *  The text is plain text: every ASCII punctuation byte in it is backslash-escaped, which
 *  closes markdown's own constructs, so nothing a renderer is given can become a link, an
 *  image, a tag or a code span - not by itself, and not by the end of one frame meeting the
 *  start of the next. An escape closes none of GFM's literal autolinks, which a renderer finds
 *  after it has decoded the escapes, so a word a renderer would link on its own - a `www.`
 *  host, a scheme's `://`, a `mailto:` or `xmpp:`, an address - is dropped from the answer
 *  instead, whole and however many pieces of the model's text it was written in. A `text` frame
 *  therefore never carries a link; the `item` frame is where the links of an answer live.
 *  Generated into Go as `contracts.GoingEventText`. */
export const GOING_EVENT_TEXT = "text";

/** The frame naming one thing the answer cites, so the page can link it. Its `href` is the
 *  cited item's own href, and it is the one link the stream carries: the server emits the
 *  link's visible text as escaped plain text and this frame beside it.
 *  Generated into Go as `contracts.GoingEventItem`. */
export const GOING_EVENT_ITEM = "item";

/** The frame saying the answer was cut at `GOING_ANSWER_WORD_MAX` words.
 *  Generated into Go as `contracts.GoingEventCut`. */
export const GOING_EVENT_CUT = "cut";

/** The frame saying the server did not pass the answer on as the model wrote it. `dropped` is
 *  how many pieces of it that came to: a link or image reduced to the text a reader sees of it,
 *  a dropped tag or autolink, and a word GFM would have linked on its own. A link to an item
 *  the model was given is no such change: the `item` frame carries it.
 *  Generated into Go as `contracts.GoingEventChanged`. */
export const GOING_EVENT_CHANGED = "changed";

/** The frame saying the model produced no answer text at all. An answer that kept no link is
 *  not this, and neither is one the server reduced or dropped every word of: each is shown
 *  with what it came to, and its `end` frame's `items` of 0 says it rests on no source.
 *  Generated into Go as `contracts.GoingEventNone`. */
export const GOING_EVENT_NONE = "none";

/** The frame ending a stream that answered, with what the answer came to.
 *  Generated into Go as `contracts.GoingEventEnd`. */
export const GOING_EVENT_END = "end";

/** The frame ending a stream that failed, carrying one of the `GOING_ERROR_*` codes.
 *  Generated into Go as `contracts.GoingEventError`. */
export const GOING_EVENT_ERROR = "error";

/** The error code of a run the model never answered inside `GOING_RUN_MS`.
 *  Generated into Go as `contracts.GoingErrorAnswerDidNotArrive`. */
export const GOING_ERROR_ANSWER_DID_NOT_ARRIVE = "ANSWER_DID_NOT_ARRIVE";

/** The error code of a deployment with no text model configured, which is the one failure the
 *  page explains rather than retries. Generated into Go as
 *  `contracts.GoingErrorNoModelConfigured`. */
export const GOING_ERROR_NO_MODEL_CONFIGURED = "NO_MODEL_CONFIGURED";

/** The error code of a run the model service refused or failed; it names no detail, because a
 *  model service's own text is not shown to a reader. Generated into Go as
 *  `contracts.GoingErrorAnswerFailed`. */
export const GOING_ERROR_ANSWER_FAILED = "ANSWER_FAILED";

/** The name of the one tool the model is given: another search of Dispatch.
 *  Generated into Go as `contracts.GoingSearchTool`. */
export const GOING_SEARCH_TOOL = "search_dispatch";

/** The most words an answer carries; past it the stream cuts the answer and says so with a
 *  `cut` frame. Generated into Go as `contracts.GoingAnswerWordMax`. */
export const GOING_ANSWER_WORD_MAX = 120;

/** The most of one hit's own text the model is shown, in runes, so a single long document cannot
 *  fill the run's budget. Generated into Go as `contracts.GoingHitRuneMax`. */
export const GOING_HIT_RUNE_MAX = 2000;

/** The most hit text one run shows the model in total, in runes; a hit past it is still named to
 *  the model, with its kind, id and title and no body, and stays something the answer may cite.
 *  Generated into Go as `contracts.GoingRequestRuneMax`. */
export const GOING_REQUEST_RUNE_MAX = 30000;

/** The most searches one run makes, the server's own seeding search counting as the first, so the
 *  model may search twice more. Generated into Go as `contracts.GoingSearchMax`. */
export const GOING_SEARCH_MAX = 3;

/** The most calls one run makes to the model, which bounds a model that keeps asking for another
 *  search instead of answering. Generated into Go as `contracts.GoingModelCallMax`. */
export const GOING_MODEL_CALL_MAX = 4;

/** The longest question the route accepts, in UTF-16 units.
 *  Generated into Go as `contracts.GoingWordsMax`. */
export const GOING_WORDS_MAX = 2000;

/** How long one run may take, measured from the POST that started it; past it the stream ends
 *  with `GOING_ERROR_ANSWER_DID_NOT_ARRIVE`. Generated into Go as `contracts.GoingRunWindow`. */
export const GOING_RUN_MS = 30_000;

/** How long a run's rows are kept, so a reader that reconnects still finds its answer and a
 *  finished run is not kept for ever. Generated into Go as `contracts.GoingRequestTTL`. */
export const GOING_REQUEST_TTL_MS = 3_600_000;

/** The next piece of the answer's text, as plain text whose every ASCII punctuation byte is
 *  backslash-escaped, so a renderer makes no link, image, tag or code span of any of it, and
 *  with every word a renderer would link on its own dropped rather than sent. */
export interface GoingTextEvent {
  readonly event: typeof GOING_EVENT_TEXT;
  readonly data: { readonly text: string };
}

/** One thing the answer cites: its kind and id name it, `href` links it and `title` names it to a
 *  reader. This is the one frame of the stream that carries a link. `title`, `kind` and `id` are
 *  plain text, which a reader is shown as text and never read as markdown. */
export interface GoingItemEvent {
  readonly event: typeof GOING_EVENT_ITEM;
  readonly data: {
    readonly kind: string;
    readonly id: string;
    readonly href: string;
    readonly title: string;
  };
}

/** The answer was cut; `words` is how many words it carries. */
export interface GoingCutEvent {
  readonly event: typeof GOING_EVENT_CUT;
  readonly data: { readonly words: number };
}

/** The server did not pass the answer on as the model wrote it; `dropped` is how many pieces of
 *  it that came to, links and images reduced to their text among them. */
export interface GoingChangedEvent {
  readonly event: typeof GOING_EVENT_CHANGED;
  readonly data: { readonly dropped: number };
}

/** The model produced no answer text at all. */
export interface GoingNoneEvent {
  readonly event: typeof GOING_EVENT_NONE;
  readonly data: Record<string, never>;
}

/** The stream ended with an answer, of `words` words citing `items` things. */
export interface GoingEndEvent {
  readonly event: typeof GOING_EVENT_END;
  readonly data: { readonly words: number; readonly items: number };
}

/** The stream ended with a failure, named by one of the `GOING_ERROR_*` codes. */
export interface GoingErrorEvent {
  readonly event: typeof GOING_EVENT_ERROR;
  readonly data: { readonly code: string };
}

/** Every frame a reader of the stream sees; `end` and `error` are the two that end it. */
export type GoingEvent =
  | GoingTextEvent
  | GoingItemEvent
  | GoingCutEvent
  | GoingChangedEvent
  | GoingNoneEvent
  | GoingEndEvent
  | GoingErrorEvent;
