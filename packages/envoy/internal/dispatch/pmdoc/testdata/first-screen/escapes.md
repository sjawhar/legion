# Characters a page must not read as markup

A paragraph with &, <, >, " and ' in it, and a <b>span of raw HTML</b> beside them.

> A quotation, with `a & b < c` inside it.
> Its second line.

---

A picture of the web: ![a chart](https://example.test/chart.png "A chart").

A picture of this site: ![a diagram](dispatch://CORE-1/artifact/diagram-png).

A footnote reference[^note], and its note below.

[^note]: The note's own text, with <em>markup</em> in it.

```
A fence with no language, holding <script>alert("x")</script>.
```
