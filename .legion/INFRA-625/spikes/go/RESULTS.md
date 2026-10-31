# GoPmdocLists: pmdoc at the 32,000-char cap (INFRA-625)

Scratch copies: `envoy-base` (= workspace main + goldmark v1.8.6 pristine), `envoy` (+ `pmdoc.diff`,
`goldmark.diff`). `go env GOCACHE GOMODCACHE` were already `/legion/go/{build,mod}` and were reused.
Measured with `TestSpikeShapes` (`cpuTimeOf`, GC stopped, min of N), 3-CPU pod under ambient load
from sibling spikes; base and patched runs interleaved round-robin (`rounds.sh`).

## Full shape sweep, min of 5, normal mode (ms CPU) - base

| shape | Parse | ParseForWrite | ParseFragment |
| --- | --- | --- | --- |
| list_plain `- x\n`x8000 | 150 | 340 | 150 |
| list_blank `- x\n\n`x6400 | 120 | 330 | 120 |
| ordered `N. x\n` to 32 KB | 70 | 180 | 70 |
| nest49_blanks | 320 | 320 | 330 |
| nest50_blanks | 360 | 340 | 330 |
| nest49_x | 40 | 50 | 50 |
| nest50_x | 50 | 50 | 50 |
| dash49_blanks `- `x49+`x` then blanks | 410 | 430 | 350 |
| dash49_x | 40 | 50 | 40 |
| quote_lazy `> x\n`+`x\n` | 20 | 20 | 20 |
| brackets `[`x15000+`]`x15000 | 230 | 460 | 220 |
| brackets_callout | refused (ErrSchema, typed directive) | | |
| para_lines `x\n`x16000 | 20 | 30 | 10 |

nest50 == nest49: `nestingGuard` refuses past maxNesting=100 blocks (a list and its item each count
one), so both open 49 levels.

## Headline shapes, min of 21 samples (3 interleaved rounds x 7), normal mode

| shape | Parse base -> fix | ForWrite base -> fix | Fragment base -> fix |
| --- | --- | --- | --- |
| list_plain | 130 -> 40 | 400 -> 170 | 160 -> 30 |
| nest49_blanks | 390 -> 260 | 340 -> 280 | 400 -> 240 |
| dash49_blanks | 450 -> 310 | 410 -> 320 | 410 -> 260 |
| brackets | 270 -> 60 | 480 -> 160 | 230 -> 80 |

## Same shapes under -race (CGO_ENABLED=1), min of 6 samples (2 rounds x 3)

| shape | Parse base -> fix | ForWrite base -> fix | Fragment base -> fix |
| --- | --- | --- | --- |
| list_plain | 750 -> 710 | 2130 -> 1780 | 870 -> 720 |
| nest49_blanks | 5490 -> 3960 | 5180 -> 3790 | 4990 -> 3960 |
| dash49_blanks | 5240 -> 4630 | 5430 -> 4480 | 5110 -> 3400 |
| brackets | 4150 -> 1710 | 8440 -> 2560 | 4290 -> 1310 |

Full -race sweep at base (min of 5): list_plain 860/1930/910, list_blank 690/1580/720,
ordered 470/1210/470, nest49_blanks 5310/4840/4010, nest50_blanks 4380/4290/4500, nest49_x
400/470/410, dash49_blanks 4720/4650/4440, dash49_x 420/530/470, quote_lazy 240/290/260,
brackets 4060/7590/4180, para_lines 260/300/210.

## Benchmark wall time (`-benchtime 10x`, includes GC), nest49_blanks Parse

647 ms/op base -> 535 ms/op (goldmark blankLines) -> 248 ms/op (all fixes).

## Raw logs

`normal.rounds`, `race.rounds`, `envoy-base.dump` / `envoy.dump` (tree hashes), `prof/*.prof`.
