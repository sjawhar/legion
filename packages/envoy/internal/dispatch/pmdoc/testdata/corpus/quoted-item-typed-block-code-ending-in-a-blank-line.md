Indented code ending a typed block no fence closes, in a quoted list item, before one blank line of
the quote with nothing in the document past the block: the browser editor's parser ends the block
before that line, which ends the list item, so it spaces nothing.

> - a
>   :::callout{#c1 kind="note" title="T"}
>       code
>
