| metric | before: median [range] (n=5) | after: median [range] (n=5) |
|---|---|---|
| box_busy_before_% | 0.700 [0.500 – 0.800] | 0.500 [0.300 – 0.900] |
| offered/s | 39.89 [39.89 – 39.89] | 39.89 [39.89 – 39.89] |
| committed/s | 0.527 [0.455 – 0.538] | 0.483 [0.400 – 0.543] |
| arrival p50 ms | 21,376 [20,880 – 23,738] | 19,888 [18,442 – 21,838] |
| arrival p99 ms | 30,982 [30,394 – 31,772] | 31,843 [30,120 – 31,973] |
| commit p50 ms | 79.08 [76.79 – 91.34] | 87.01 [83.43 – 97.50] |
| commit p99 ms | 3,462 [3,369 – 3,720] | 3,949 [2,523 – 4,216] |
| publish p50 ms | - | 6.15 [6.04 – 6.50] |
| publish p99 ms | - | 44.00 [39.45 – 70.67] |
| publish max ms | - | 107 [52.62 – 217] |
| receipts expected | 1,580 [1,365 – 1,615] | 1,450 [1,200 – 1,630] |
| receipts got | 1,580 [1,365 – 1,615] | 1,450 [1,200 – 1,630] |
| lost (expected-got) | 0 [0 – 0] | 0 [0 – 0] |
| listener cores | 0.047 [0.045 – 0.050] | 0.062 [0.059 – 0.063] |
| listener RSS max MiB | 45.87 [42.35 – 48.61] | 93.85 [89.61 – 102] |
| nats cores | 0.008 [0.008 – 0.010] | 0.025 [0.024 – 0.026] |
| nats RSS max MiB | 64.82 [61.12 – 65.63] | 160 [159 – 165] |
| dispatch cores | 0.293 [0.285 – 0.300] | 0.284 [0.280 – 0.289] |
| loadgen cores | 0.514 [0.499 – 0.538] | 0.515 [0.489 – 0.534] |
| box busy % | 61.50 [61.12 – 62.03] | 61.73 [61.55 – 61.89] |
| stack exits | 0 [0 – 0] | 0 [0 – 0] |
| listener cores 1st/last third | 0.046 [0.045 – 0.051] / 0.047 [0.045 – 0.051] | 0.061 [0.058 – 0.064] / 0.063 [0.057 – 0.065] |
| listener RSS MiB 1st/last third | 39.99 [37.85 – 44.01] / 43.72 [39.92 – 45.52] | 82.70 [79.23 – 84.63] / 84.79 [83.56 – 86.56] |
| listener RSS slope MiB/min | 0.302 [0.027 – 0.633] | 0.409 [-0.183 – 0.680] |
| nats cores 1st/last third | 0.009 [0.008 – 0.010] / 0.008 [0.008 – 0.010] | 0.027 [0.024 – 0.030] / 0.025 [0.022 – 0.025] |
| nats RSS MiB 1st/last third | 52.56 [50.41 – 53.41] / 61.26 [57.44 – 63.68] | 143 [141 – 145] / 156 [155 – 157] |
| nats RSS slope MiB/min | 1.31 [0.937 – 1.57] | 1.86 [1.71 – 2.36] |
| consumers max | 4 [4 – 5] | 1,004 [1,004 – 1,004] |
| pull req/s | 0 [0 – 0] | 33.33 [33.33 – 33.33] |
| session pull req/s | 0 [0 – 0] | 33.33 [33.33 – 33.33] |
| JS API req/s | 0.153 [0.153 – 0.153] | 22.43 [22.42 – 22.52] |
| registered | 1,000 [1,000 – 1,000] | 1,000 [1,000 – 1,000] |
| heartbeat errors | 0 [0 – 0] | 0 [0 – 1] |
| nats slow consumers | 0 [0 – 0] | 0 [0 – 0] |
| sessions on consumer | - | 1,000 [1,000 – 1,000] |
| sessions on core | - | 0 [0 – 0] |
| relay frames | - | 1,747 [1,589 – 2,097] |
| duplicates | - | 0 [0 – 0] |
| redelivered | - | 0 [0 – 0] |

### before: per run

| metric | before-r1 | before-r2 | before-r3 | before-r4 | before-r5 |
|---|---|---|---|---|---|
| box_busy_before_% | 0.500 | 0.600 | 0.800 | 0.700 | 0.700 |
| offered/s | 39.89 | 39.89 | 39.89 | 39.89 | 39.89 |
| committed/s | 0.455 | 0.527 | 0.522 | 0.538 | 0.528 |
| arrival p50 ms | 23,738 | 21,376 | 20,992 | 22,582 | 20,880 |
| arrival p99 ms | 31,772 | 30,394 | 30,758 | 31,242 | 30,982 |
| commit p50 ms | 91.34 | 76.79 | 78.63 | 79.08 | 81.01 |
| commit p99 ms | 3,679 | 3,462 | 3,369 | 3,720 | 3,395 |
| publish p50 ms | - | - | - | - | - |
| publish p99 ms | - | - | - | - | - |
| publish max ms | - | - | - | - | - |
| receipts expected | 1,365 | 1,580 | 1,565 | 1,615 | 1,585 |
| receipts got | 1,365 | 1,580 | 1,565 | 1,615 | 1,585 |
| lost (expected-got) | 0 | 0 | 0 | 0 | 0 |
| listener cores | 0.050 | 0.047 | 0.045 | 0.049 | 0.046 |
| listener RSS max MiB | 42.94 | 45.87 | 46.57 | 48.61 | 42.35 |
| nats cores | 0.008 | 0.008 | 0.009 | 0.010 | 0.008 |
| nats RSS max MiB | 65.63 | 61.76 | 64.82 | 61.12 | 65.40 |
| dispatch cores | 0.297 | 0.293 | 0.288 | 0.300 | 0.285 |
| loadgen cores | 0.514 | 0.527 | 0.499 | 0.538 | 0.512 |
| box busy % | 61.51 | 61.50 | 61.35 | 62.03 | 61.12 |
| stack exits | 0 | 0 | 0 | 0 | 0 |
| listener cores 1st/last third | 0.051 / 0.048 | 0.048 / 0.046 | 0.045 / 0.045 | 0.046 / 0.051 | 0.045 / 0.047 |
| listener RSS MiB 1st/last third | 37.85 / 39.92 | 40.92 / 43.72 | 39.80 / 44.00 | 44.01 / 45.52 | 39.99 / 40.18 |
| listener RSS slope MiB/min | 0.302 | 0.415 | 0.633 | 0.222 | 0.027 |
| nats cores 1st/last third | 0.008 / 0.008 | 0.009 / 0.008 | 0.009 / 0.010 | 0.010 / 0.010 | 0.009 / 0.008 |
| nats RSS MiB 1st/last third | 52.56 / 63.27 | 50.41 / 57.44 | 52.56 / 61.26 | 51.38 / 57.47 | 53.41 / 63.68 |
| nats RSS slope MiB/min | 1.57 | 1.05 | 1.31 | 0.937 | 1.48 |
| consumers max | 4 | 5 | 4 | 4 | 4 |
| pull req/s | 0 | 0 | 0 | 0 | 0 |
| session pull req/s | 0 | 0 | 0 | 0 | 0 |
| JS API req/s | 0.153 | 0.153 | 0.153 | 0.153 | 0.153 |
| registered | 1,000 | 1,000 | 1,000 | 1,000 | 1,000 |
| heartbeat errors | 0 | 0 | 0 | 0 | 0 |
| nats slow consumers | 0 | 0 | 0 | 0 | 0 |
| sessions on consumer | - | - | - | - | - |
| sessions on core | - | - | - | - | - |
| relay frames | - | - | - | - | - |
| duplicates | - | - | - | - | - |
| redelivered | - | - | - | - | - |

### after: per run

| metric | after-r1 | after-r2 | after-r3 | after-r4 | after-r5 |
|---|---|---|---|---|---|
| box_busy_before_% | 0.300 | 0.300 | 0.500 | 0.700 | 0.900 |
| offered/s | 39.89 | 39.89 | 39.89 | 39.89 | 39.89 |
| committed/s | 0.400 | 0.535 | 0.543 | 0.483 | 0.483 |
| arrival p50 ms | 18,442 | 21,838 | 21,690 | 19,415 | 19,888 |
| arrival p99 ms | 31,611 | 30,120 | 31,940 | 31,973 | 31,843 |
| commit p50 ms | 83.43 | 84.33 | 87.01 | 88.35 | 97.50 |
| commit p99 ms | 4,216 | 2,523 | 3,949 | 4,057 | 3,383 |
| publish p50 ms | 6.11 | 6.19 | 6.04 | 6.50 | 6.15 |
| publish p99 ms | 56.41 | 70.67 | 39.45 | 44.00 | 42.79 |
| publish max ms | 217 | 107 | 52.62 | 136 | 54.91 |
| receipts expected | 1,200 | 1,605 | 1,630 | 1,450 | 1,450 |
| receipts got | 1,200 | 1,605 | 1,630 | 1,450 | 1,450 |
| lost (expected-got) | 0 | 0 | 0 | 0 | 0 |
| listener cores | 0.062 | 0.059 | 0.061 | 0.062 | 0.063 |
| listener RSS max MiB | 95.61 | 89.61 | 102 | 93.64 | 93.85 |
| nats cores | 0.026 | 0.024 | 0.024 | 0.025 | 0.026 |
| nats RSS max MiB | 159 | 165 | 160 | 161 | 160 |
| dispatch cores | 0.289 | 0.280 | 0.284 | 0.280 | 0.285 |
| loadgen cores | 0.522 | 0.489 | 0.506 | 0.515 | 0.534 |
| box busy % | 61.62 | 61.55 | 61.73 | 61.88 | 61.89 |
| stack exits | 0 | 0 | 0 | 0 | 0 |
| listener cores 1st/last third | 0.060 / 0.062 | 0.061 / 0.057 | 0.058 / 0.064 | 0.063 / 0.065 | 0.064 / 0.063 |
| listener RSS MiB 1st/last third | 80.67 / 85.09 | 79.23 / 83.85 | 83.36 / 86.56 | 82.70 / 84.79 | 84.63 / 83.56 |
| listener RSS slope MiB/min | 0.680 | 0.646 | 0.409 | 0.337 | -0.183 |
| nats cores 1st/last third | 0.027 / 0.025 | 0.028 / 0.022 | 0.024 / 0.025 | 0.027 / 0.025 | 0.030 / 0.024 |
| nats RSS MiB 1st/last third | 143 / 155 | 145 / 157 | 144 / 156 | 143 / 155 | 141 / 156 |
| nats RSS slope MiB/min | 1.71 | 1.82 | 1.86 | 1.92 | 2.36 |
| consumers max | 1,004 | 1,004 | 1,004 | 1,004 | 1,004 |
| pull req/s | 33.33 | 33.33 | 33.33 | 33.33 | 33.33 |
| session pull req/s | 33.33 | 33.33 | 33.33 | 33.33 | 33.33 |
| JS API req/s | 22.42 | 22.52 | 22.42 | 22.48 | 22.43 |
| registered | 1,000 | 1,000 | 1,000 | 1,000 | 1,000 |
| heartbeat errors | 0 | 0 | 1 | 0 | 0 |
| nats slow consumers | 0 | 0 | 0 | 0 | 0 |
| sessions on consumer | 1,000 | 1,000 | 1,000 | 1,000 | 1,000 |
| sessions on core | 0 | 0 | 0 | 0 | 0 |
| relay frames | 1,589 | 2,097 | 2,007 | 1,747 | 1,724 |
| duplicates | 0 | 0 | 0 | 0 | 0 |
| redelivered | 0 | 0 | 0 | 0 | 0 |
