Text.

> - a
>
> - b

Text.

> - a
>
>
> After.

Text.

> - a
>
> > q

Text.

> - a
>   - n
>
>
> - b
>   - n
>
> After.

Text.

> - a
> ```
> code
> ```

Text.

> :::callout{#c1 kind="note" title="T"}
> - a
> ***
> :::

Text.

> ::::callout{#c2 kind="note" title="T"}
> - a
> :::callout{#c3 kind="note" title="T"}
> inner
> :::
> ::::

See the notes[^1][^2][^3].

[^1]: One.

    - a

    - b

[^2]: Two.

    - a

    > q

[^3]: Three.

    - a
      - n

    > q

Text.

> - a
>   > q
>
> - b

Text.

> - x
>   - a
>   - b
>
>   > q

Text.

> ::::callout{#c4 kind="note" title="T"}
> - a
>   - n
>
> - b
> ::::

Text.

> - a
>
> [^4]: A definition after a list.

Text.

> - a
>
>

After the quote.

Text[^5].

[^5]: Five.

    - a

      c
    - b

Text.

- [^6]: A definition in an item.

      More.

Text[^6].

- First
  - dash
  ---

Text.

> ::::callout{#c5 kind="note" title="T"}
> - a
>
>
> ::::

Text.

> :::callout{#qc kind="note" title="T"}
> - a
>
> text
> :::

In a typed block in a definition[^5].

[^5]: :::callout{#fc kind="note" title="T"}
    - a
    -
    text
    :::

After a typed block in a definition[^6].

[^6]: :::callout{#fd kind="note" title="T"}
    - ```
      code
      ```
    :::

    After the callout.

A list in a nested quote, before the outer quote's blank lines:

> > - p
>
>
> tail

And at the outer quote's end:

> > - a
> > - b
>
>

The inner quote's own blank lines, then the outer's:

> > - s
> >
> >
>
> tail
