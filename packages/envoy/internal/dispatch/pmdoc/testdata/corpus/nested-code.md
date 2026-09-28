> - item
>
>       code

Fenced code ending in a blank line:

```
first

```

- In an item:

  ```
  code


  ```

> An unclosed fence in a quote:
>
> ```
> x
>
>

Text.

- An unclosed fence in an item:

  ```
  y


Text after the list.

An unclosed fence takes the blank line before the next item:

- ```
  open

- Next.

Text.

> - An item in a quote:
>
>   ```
>   open
>
>

Text.

- An item holding a list whose fence nothing closes:
  - ```
    open

  tail

Text.

- - ```
    open

  tail
