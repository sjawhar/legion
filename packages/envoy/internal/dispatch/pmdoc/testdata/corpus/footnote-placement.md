Text[^a], then[^c], then[^b].

> Quoted[^q].
>
> [^q]: In the quote.
>
>     - a
>     - b
>
>
>     After the list, in the definition.

[^b]: Referenced last, written first.

    - l

After the definition.

[^u]: Nothing refers to this.

Between.

- An item[^i].

  [^i]: In the item.

[^a]: Referenced first.

    > - x
    >
    >
    > In the quote.

[^c]: C.

Items holding definitions[^li] that end in a quote[^lj].

- [^li]: - a
      - b

      > q
  tail
- [^lj]: - a

      c

In a quote, an item holding a definition[^lq] whose list a paragraph follows:

> - [^lq]: - a
>
>       tail

An item holding a definition[^uf] that ends in a fence nothing closes, and a paragraph after it:

* [^uf]: ```
      x

  tail

One whose list goes on[^ug]:

* [^ug]: ```
      x

* b

And one in a quote[^uh]:

> - [^uh]: ```
>       x
>
>   tail

And one whose fence is in a list in the definition[^ui]:

* [^ui]: - ```
        x

  tail

One whose fence is in a quote in the definition[^uj], which ends at the blank line:

* [^uj]: > ```
      > x

  tail

A tight item holding a definition[^uk] with a list and a code block:

- [^uk]: - a
      ```
      x
      ```
  tail
