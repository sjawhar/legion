Callouts in list items in footnote definitions, each holding a fence no fence closes, and the blank lines that space or do not space their items.

Text[^a][^b][^c][^d].

[^a]: lead

    - x
      :::callout{#ds1 kind="note" title="T"}
      ~~~
      code
      :::

    - y

[^b]: lead

    - x
      :::callout{#ds2 kind="note" title="T"}
      ```
      code

    - y

> [^c]: lead
>
>     - x
>       :::callout{#ds3 kind="note" title="T"}
>       ~~~
>       code
>       :::
>
>     - y

> [^d]: lead
>
>     - x
>       :::callout{#ds4 kind="note" title="T"}
>       ~~~
>       code
>
>       :::
>
>
>     after
