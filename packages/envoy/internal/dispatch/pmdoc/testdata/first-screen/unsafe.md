# What a document may not do to the page it is drawn into

A paragraph holding <script>alert("one")</script> as text, and `</p><script>` in code.

A [link a reader must not run](javascript:alert('two')), beside a
[link they may follow](https://example.test/safe) and a [reference](dispatch://CORE-1/spec).

A picture whose source must not run: ![nothing](javascript:alert('three')).

> A quotation holding </blockquote> and an & ampersand.

```html
<script>alert("four")</script>
```
