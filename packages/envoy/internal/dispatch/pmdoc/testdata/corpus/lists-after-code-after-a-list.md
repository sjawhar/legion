Lists after an indented code block that follows a list.

Next.

1.

    code
2. b

Next.

-

    1
-

Next.

-

    1

-

Next.

> 1.
>
>     code
> 2. b

Next.

1.


    code
2. b

Next.

- a
-

    code
2. b

Next.

:::callout{#b1 kind="note" title="T"}
1.

    code
2. b
:::

Next.

1.

    code
# h

    code2
2. b

Next.

# h

    code
2. b

Next.

The same after a footnote definition, which goldmark gathers apart while the document parses:

x[^n]

[^n]: d

10. 


    # h
11. x

Next.

Next.

[^o]: d

1.

    code
2. b

Next.

- a

[^m]: d

1.

    code
2. b
