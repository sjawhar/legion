---
name: dispatch-view
description: "Use when you write or change an HTML page that a Dispatch view block draws, when you add a view block to a document, comment, ask or message, or when a view shows a line instead of a drawing: no data, no grant, a page that does not start, or a frame of the wrong height."
---

# A Dispatch view block and the page it draws

A `view` block shows one HTML page of yours in a sandboxed frame, inside a Dispatch document, a
comment, a reply, an ask or a message. Dispatch sends the page the newest version of each data
file a person granted it, and sends each later version within 5 seconds of its upload. The page
draws. Dispatch draws nothing: it holds no chart library and parses none of your data.

Write the page as a file in Dispatch. Write the data as files in Dispatch. Then write the block.

## The block

```md
:::view{page="dispatch://INFRA-682/artifact/chart-html" data="dispatch://INFRA-682/artifact/data-json dispatch://INFRA-682/artifact/more-json"}
Attempts of the kernel boot loop, from the newest data.json.
:::
```

- `page`: one reference to the HTML page file.
- `data`: one to eight references to data files, with one space between two references.
- The body is the caption. It is required. Dispatch shows it above the frame and uses it as the
  frame's title. A block with an empty caption shows the line "this view has no caption".

A reference names an issue file (`dispatch://KEY/artifact/<slug>`) or a project file
(`dispatch://PROJECT/artifact/<slug>`). Add `@vN` to pin one version. A reference without `@vN`
follows the newest version, which is what makes the drawing live.

These are not files a `view` block can name: a document, a file of an agent conversation
(`dispatch://agent/<session>/artifact/<slug>`), an issue, an ask, a comment, a block of a
document. Each shows a line instead of a drawing.

Upload the page file and the data files with `dispatch artifact`. A page file is an issue's or a
project's file whose **latest** version's stored type is `text/html`, or whose name ends in
`.html` or `.htm`; a document is never one, and neither is a file of an agent's conversation.
The latest version decides it even when a block names an older `@vN`, because a grant hangs from
the file rather than from a version. So uploading a version of another type over a page file
stops it being one: its block stops drawing and its grants stop being listed. A data file is a
file of any type, but a page reads only a file whose bytes are UTF-8 text.

## The grants

Dispatch gives a page only the data files a person granted to that page file. The grant belongs to
the page file, not to the block, because any author writes a block and only a person writes a
grant.

- A person adds and removes grants in the panel "Data this page may read", on the page file's own
  view: `/issues/{key}/artifacts/{slug}` for an issue file, `/projects/{key}/documents/{slug}` for
  a project file.
- A grant without a version covers every version of that data file, the newest included. A grant
  with a version covers that version only.
- A grant covers every version of the page file. A new page version keeps the grants.
- A data reference that pins `@vN` is covered by a grant without a version, or by a grant pinned
  to the same N.
- A change of grants reaches every open block within 5 seconds. A block whose covered files change
  starts a new frame, so a page that lost a grant also loses the data it held.

**You cannot grant a file. A token cannot.** Every token gets `403 HUMAN_ONLY` from the grant
routes. Ask the person who owns the issue, and say which page file needs which data files:

```bash
dispatch ask --issue INFRA-682 --urgency med --question-file - <<'EOF'
Please grant two data files to the page file chart.html of dispatch://INFRA-682, so the view block
in the spec draws. Open /issues/INFRA-682/artifacts/chart-html, panel "Data this page may read",
and add dispatch://INFRA-682/artifact/data-json and dispatch://INFRA-682/artifact/more-json. Only
a person can: the route answers a token 403 HUMAN_ONLY.
EOF
```

Until a person answers, each ungranted file reaches the page as `{"error": "not-granted"}`, and
the reader sees the line "not granted to this page" with a link to the panel. The page must draw
that entry without throwing.

## The four messages

Dispatch and the page talk with `postMessage`. The page posts to `window.parent`, with the
target origin `"*"`, because its own origin is opaque. Dispatch answers the page's first `ready`
with a `MessagePort`, and every data message comes over that port and over nothing else.

| Message | From | Carried on | Shape |
| --- | --- | --- | --- |
| ready | page | `window.parent` | `{type: "dispatch-view:ready"}` |
| port | Dispatch | the frame's window, once | `{type: "dispatch-view:port"}`, with the port in `event.ports[0]` |
| data | Dispatch | that port | `{type: "dispatch-view:data", files: [...]}` |
| height | page | `window.parent` | `{type: "dispatch-view:height", height: <CSS pixels>}` |

- The page posts `dispatch-view:ready` when it listens. Dispatch answers only the first `ready` of
  a frame. A page that posts no `ready` within 10 seconds of its load gets no data, and the reader
  sees the line "this page sent no ready message in 10 seconds".
- That one answer is `dispatch-view:port`, and it carries the page's `MessagePort` in
  `event.ports[0]`. Keep it: it is the only way data reaches the page. A transferred port belongs
  to the document that took it, so a page that moves its frame leaves the port behind with its
  own document, and the document that replaces it is given none — that is how Dispatch sends a
  page that moved away nothing more.
- Dispatch posts `dispatch-view:data` on that port after the handover, and again, with every
  file, after any data file gets a new version. Dispatch posts to one frame at most once in 2
  seconds, sends only the newest version of each file, and never posts an older version after a
  newer one. The page gets many of these messages. Draw each one again.
- The page posts `dispatch-view:height` with its height in CSS pixels. Dispatch takes a finite
  number only, holds it between 100 and 2,000, and applies at most one height per animation frame
  and at most 200 heights per frame. Before the first height, the frame is 400 pixels tall.

**Read only a message whose `event.source` is `window.parent`.** Another block's page runs in the
same reader and can post to yours. A page that reads any message takes a port from whoever offers
one, and draws whatever that page sends it. Nothing but Dispatch can post on your port once you
hold it.

```js
window.addEventListener("message", (event) => {
  if (event.source !== window.parent) {
    return;
  }
  const message = event.data;
  if (message === null || typeof message !== "object") {
    return;
  }
  if (message.type === "dispatch-view:port") {
    event.ports[0].onmessage = (sent) => {
      if (sent.data.type === "dispatch-view:data") {
        draw(sent.data.files);
      }
    };
  }
});
```

## The entries of a data message

`files` holds one entry per data file, in the order of the `data` attribute. A granted file that
Dispatch read is:

```json
{
  "ref": "dispatch://INFRA-682/artifact/data-json",
  "name": "data.json",
  "version": 7,
  "contentType": "application/json",
  "text": "[{\"label\":\"boot\",\"value\":12}]"
}
```

`text` is the version's bytes as UTF-8 text. Dispatch parses nothing, so the page reads JSON, CSV
or any other text itself.

Every other entry carries an `error` code and no `text`. Handle all four:

| `error` | What happened | What the page does |
| --- | --- | --- |
| `not-granted` | No grant of this page covers the file. | Say that a person must add the grant. |
| `unreadable` | Dispatch could not resolve the file: it is not found, it has no version, it is a document, or the reference names no issue file and no project file. | Say the file did not arrive. |
| `not-text` | The version's bytes are not UTF-8 text, as a picture is not. | Say the file is not text. |
| `read-failed` | The read of the bytes failed. The entry carries `status` when Dispatch got an answer, and no `status` when the request itself failed. | Say the read failed, and show `status` when it is there. |

Every entry carries `ref`. An entry with an `error` carries no `name` and no `version`, so name it
by its `ref`. One file that fails does not stop the others: the message still carries every entry.
The reader also sees a line beside the frame for each of these, so the page does not have to
explain the cause. It must not throw.

## What the page may do, and may not

The page runs in an opaque origin, under a `Content-Security-Policy` the page route sends:
`sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob:;
style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; worker-src
blob:; connect-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors
'self'`.

- The page may run inline script, apply inline style, draw with the DOM, canvas, SVG or WebGL, and
  load a picture, a font, a sound or a worker from `data:` or `blob:`.
- The page may not fetch anything. `fetch`, `XMLHttpRequest`, `EventSource`, `WebSocket`,
  `sendBeacon`, a form, a script tag, a stylesheet link and a picture from a URL all fail. Put
  every library and every asset the page needs in the page's own bytes.
- The page holds no cookie and no storage of Dispatch's. It cannot read the reader's session, the
  Dispatch page around it, or another frame. It cannot open a window or move the reader's tab.
- **The page must not move its own frame.** A link that navigates, a `location` change, a
  `form` that submits, a reload: each is a second load of the frame. The page's port goes with
  the document it replaced, so no data reaches the new one, whether or not it ever finishes
  loading; where the second load does complete, Dispatch also removes the frame for good and
  shows "the page moved away from its address". The reader gets the drawing back only by loading
  the Dispatch page again. Give every link `target="_blank"`, or draw no link at all.
- Treat the text of a data file as text, never as markup. Write it with `textContent`, and build
  nodes with `document.createElement`. A data file is written by whoever can upload to it.

## The limits

| Limit | Value |
| --- | --- |
| Data files one block names | 1 to 8 |
| Page file | at most 5 MiB (`413 PAGE_TOO_LARGE` over it, and no frame) |
| A block's granted data together | at most 16 MiB (over it Dispatch sends no data at all) |
| Frames running at once on one Dispatch page | 4, the four nearest the screen |
| A frame starts | when the block comes within one screen height of the screen |
| A frame is removed | when the block goes more than three screen heights away |
| Data messages to one frame | at most one in 2 seconds |
| Frame height | 400 pixels until the page reports one, then 100 to 2,000 |

A frame that is removed and started again loads the page from the beginning. The page keeps
nothing between two loads, so draw from the data message alone and hold no state a reload loses.

The 16 MiB limit counts only the granted files. Keep each data file small: the reader downloads
every byte of it on a phone.

## A trustworthy origin

A browser sends the `Sec-Fetch-Dest` header only to a potentially trustworthy URL: HTTPS, or
`localhost`. The page route answers `Content-Disposition: inline` only when `Sec-Fetch-Dest` is
`iframe`, and `attachment` otherwise. So **a view draws over HTTPS and on localhost. On any other
plain-HTTP deployment it does not draw.** There the frame gets a download, and the block shows a
line. This is the route's defence against a browser that opens the page's address in a tab, and
it is not a setting a page can change.

## The minimal page

[minimal-page.html](./minimal-page.html) is a working page, and the shortest one worth copying. It
posts `ready`, takes the port out of the one message it reads from `window.parent`, draws one
card per entry of each data message that port carries, says what went wrong for each of the four
`error` codes, and posts its height after each draw. It draws a JSON
array of `{"label": <string>, "value": <number>}` objects as bars, and any other text in a `<pre>`.
Replace `rowsOf` and `bars` with the drawing you need, and keep the rest.

## Before you say a view is done

1. The page file, each data file and the block name the same references, letter for letter.
2. A person granted each data file to the page file, and each grant names the file the block
   names.
3. The page posts `ready` once, keeps the port it is answered with, and posts a height after it
   draws.
4. The page draws an entry that carries each of the four `error` codes without throwing.
5. The page loads nothing from the network.
6. The caption says what the drawing shows, for a reader whose browser shows no frame.
