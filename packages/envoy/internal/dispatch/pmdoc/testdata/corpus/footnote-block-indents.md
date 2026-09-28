Refs[^1][^2][^3][^4][^5].

[^1]: ```
     x
    ```

[^2]: - a
      ```
      x
      ```

[^3]: - a

      b

[^4]: > ```
    >  x
    > ```

[^5]: 1. a
       ```
        y
       ```

A definition[^bl] whose code holds only blank lines:

[^bl]: a

    ```


    ```

And one[^bm] whose code opens with one:

[^bm]: a

    ```

    c
    ```

In a quote, a definition[^bq] whose code holds a blank line:

> [^bq]: ```
>     c
>
>     d
>     ```

And one[^br] in a quoted list item:

> - a
>   [^br]: ```
>       c
>
>       d
>       ```

And one[^bs] whose blank code line holds spaces:

> [^bs]: ```
>     c
>    
>     d
>     ```
