# Readout cost: measurements

> **Note (2026-09-26).** The legacy raw recording format (`Format ""`: raw
> segment archives and activity sidecars) has been removed. Every recording is
> visual/1 without configuration; legacy code paths, routes and tests this
> document compares against or describes as unchanged no longer exist, and a
> legacy recording is listed as unsupported and refused. See
> [ARCHIVE.md](../ARCHIVE.md).

What one readout of the pinned avt helper costs, at 1, 10 and 32 concurrent
helper processes, for five workloads and four readout modes.

**These are measurements on one host, not budgets.** Nothing here sets a
threshold, and no coalescing policy is decided here — that is later work, and it
should start from these numbers rather than from an assumption. Browser, player
and screen-capture costs are entirely excluded: nothing in this run launched a
browser. What is measured is the cost of getting a canonical `Screen` or
`ScreenDelta` out of the helper and into Go, and nothing else.

The contract being measured is defined in `visual-recording-readout.md`; this
document does not restate it.

## How to run

The devenv exports an `AREEL_TEST_AVT` that predates the readout operations, so
it must be overridden with a freshly built helper or the `V`/`W` cases fail.

```
cd tools/agent-reel
HELPER=$(nix build 'path:.#avt-checkpoint' --no-link --print-out-paths)
test -z "$(gofmt -l cmd internal)"
go vet ./internal/castexport
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint go test -count=1 -run '^TestReadoutMeasurements$' -v ./internal/castexport
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint go test -count=1 -run '^$' -bench 'Readout' -benchtime=2s ./internal/castexport
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint go test -count=1 ./internal/castexport
AREEL_TEST_AVT=$HELPER/bin/areel-avt-checkpoint CGO_ENABLED=1 go test -race -count=1 -run '^TestReadoutMeasurements$' ./internal/castexport
```

The benchmark run needs more than Go's default 10-minute binary timeout on a
slower host; add `-timeout 60m` there. Everything else fits comfortably.

Without `AREEL_TEST_AVT` every case in `readout_bench_test.go` skips, exactly
like the other helper integration tests.

## What is measured, and how

- **An event** is one `Feed` of the workload's payload followed by the readout
  mode under test. `idle` feeds nothing; `feed-only` reads nothing.
- **Instances** are separate helper processes, one per goroutine, all driven for
  the same number of events. Wall time is the whole batch, from a barrier that
  releases every goroutine at once to the last one finishing; helper startup and
  one untimed warm-up readout per helper are outside the clock.
- **events/s** is aggregate: `instances x events / wall`.
- **p50/p95/p99** are nearest-rank percentiles over every event of the batch
  (`instances x events` samples), each sample covering feed plus readout.
- **bytes/event** is *measured wire bytes*, not an estimate: the helper's
  reply stream is wrapped in a counting reader, so reply framing (geometry,
  cursor, lengths, row indices) is included along with the cells. The single
  analytic figure in the file is the max-geometry bound check, which sums the
  encoded size of each decoded cell.
- **allocs/event** is a `runtime.MemStats` `Mallocs` delta across the batch
  divided by the event count. It is process-global, so it includes the harness's
  own per-event work as well as the decoder's.
- **helper RSS** is the sum of `VmRSS` from `/proc/<pid>/status` over all helper
  processes, read at the end of the batch before any is closed. It mirrors the
  `videoexport` resource watcher's accounting without importing it.

Workloads, at 80x24 unless stated: `redraw-unchanged` (cursor-addressed
full-screen repaint of identical coloured bytes, never scrolls),
`idle` (no feed), `typing-sparse` (one printable character),
`scroll-dense` (24 full coloured lines ending in CR LF, so the screen scrolls a
page per event), `max-geometry` (512x64 = the 32,768-cell ceiling, dense
repaint). Rows carry an indexed foreground and an RGB background, so most cells
encode at 12 of the possible 14 bytes.

## Host

| | |
|---|---|
| CPU | AMD RYZEN AI MAX+ 395 w/ Radeon 8060S, `nproc` = 32 |
| Memory | 65,617,800 kB total (~62.6 GiB) |
| Kernel | 7.1.3 |
| Go | go1.26.4 linux/amd64 |
| Helper | `/nix/store/687jim2hmwmzg1j4m7mvg06fmn0rgjg8-areel-avt-checkpoint-0.1.0/bin/areel-avt-checkpoint` |
| `ulimit -n` | 524288 |
| Date | 2026-09-22 |

Nothing was exhausted at 32 instances: 32 helpers use two pipes each against an
open-file limit of 524,288, and peak summed helper RSS was 145 MiB against
62 GiB of memory. The host was not otherwise idle — it is a developer machine,
and the 32-instance rows compete with whatever else was running.

## Test matrix

`go test -run '^TestReadoutMeasurements$' -v`, 200 events per helper, total
matrix wall **8.923 s** (the whole test, including sanity checks and 2,580
helper process launches, took 8.93 s).

Sanity output from the same run:

```
max-geometry readout: 32768 cells encode 393216 payload bytes, bound 1048576
```

| workload | mode | helpers | events | wall | events/s | p50 | p95 | p99 | bytes/event | allocs/event | helper RSS |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| redraw-unchanged | feed-only | 1 | 200 | 3ms | 63522 | 1µs | 54µs | 63µs | 0 | 1.0 | 2.5 MiB |
| redraw-unchanged | feed-only | 10 | 2000 | 4ms | 454127 | 1µs | 60µs | 74µs | 0 | 1.0 | 25.2 MiB |
| redraw-unchanged | feed-only | 32 | 6400 | 11ms | 576772 | 2µs | 190µs | 423µs | 0 | 1.0 | 80.8 MiB |
| redraw-unchanged | visible | 1 | 200 | 23ms | 8541 | 105µs | 159µs | 260µs | 23061 | 3876.0 | 2.5 MiB |
| redraw-unchanged | visible | 10 | 2000 | 62ms | 32336 | 289µs | 498µs | 587µs | 23061 | 3876.1 | 25.4 MiB |
| redraw-unchanged | visible | 32 | 6400 | 156ms | 40918 | 464µs | 2.118ms | 3.11ms | 23061 | 3876.1 | 81.3 MiB |
| redraw-unchanged | changed | 1 | 200 | 23ms | 8665 | 107µs | 130µs | 245µs | 23157 | 3896.0 | 2.5 MiB |
| redraw-unchanged | changed | 10 | 2000 | 48ms | 41805 | 188µs | 445µs | 614µs | 23157 | 3896.0 | 25.5 MiB |
| redraw-unchanged | changed | 32 | 6400 | 138ms | 46433 | 454µs | 1.754ms | 2.446ms | 23157 | 3896.0 | 81.1 MiB |
| redraw-unchanged | snapshot | 1 | 200 | 10ms | 20797 | 46µs | 57µs | 81µs | 2653 | 5.0 | 2.5 MiB |
| redraw-unchanged | snapshot | 10 | 2000 | 15ms | 132268 | 71µs | 114µs | 155µs | 2653 | 5.0 | 25.8 MiB |
| redraw-unchanged | snapshot | 32 | 6400 | 37ms | 174265 | 147µs | 374µs | 561µs | 2653 | 5.0 | 82.7 MiB |
| idle | feed-only | 1 | 200 | 0s | 15632328 | 0s | 0s | 0s | 0 | 0.0 | 2.5 MiB |
| idle | feed-only | 10 | 2000 | 0s | 23987431 | 0s | 0s | 0s | 0 | 0.0 | 25.1 MiB |
| idle | feed-only | 32 | 6400 | 0s | 53879310 | 0s | 0s | 0s | 0 | 0.0 | 80.4 MiB |
| idle | visible | 1 | 200 | 20ms | 9767 | 85µs | 114µs | 261µs | 15381 | 1955.0 | 2.5 MiB |
| idle | visible | 10 | 2000 | 46ms | 43589 | 194µs | 466µs | 796µs | 15381 | 1955.0 | 25.2 MiB |
| idle | visible | 32 | 6400 | 114ms | 56245 | 319µs | 1.707ms | 2.618ms | 15381 | 1955.1 | 80.5 MiB |
| idle | changed | 1 | 200 | 1ms | 328755 | 3µs | 4µs | 10µs | 21 | 6.0 | 2.5 MiB |
| idle | changed | 10 | 2000 | 4ms | 529478 | 14µs | 38µs | 101µs | 21 | 6.0 | 25.1 MiB |
| idle | changed | 32 | 6400 | 11ms | 575151 | 18µs | 185µs | 533µs | 21 | 6.0 | 80.7 MiB |
| idle | snapshot | 1 | 200 | 1ms | 214711 | 4µs | 6µs | 14µs | 26 | 4.0 | 2.5 MiB |
| idle | snapshot | 10 | 2000 | 4ms | 519826 | 16µs | 33µs | 49µs | 26 | 4.0 | 25.1 MiB |
| idle | snapshot | 32 | 6400 | 10ms | 668987 | 21µs | 139µs | 529µs | 26 | 4.0 | 80.5 MiB |
| typing-sparse | feed-only | 1 | 200 | 0s | 2077900 | 0s | 1µs | 1µs | 0 | 2.0 | 2.5 MiB |
| typing-sparse | feed-only | 10 | 2000 | 0s | 9134380 | 0s | 1µs | 2µs | 0 | 2.0 | 25.1 MiB |
| typing-sparse | feed-only | 32 | 6400 | 0s | 14814678 | 0s | 1µs | 3µs | 0 | 2.0 | 80.8 MiB |
| typing-sparse | visible | 1 | 200 | 16ms | 12236 | 72µs | 103µs | 205µs | 15381 | 1957.0 | 2.5 MiB |
| typing-sparse | visible | 10 | 2000 | 44ms | 45877 | 195µs | 390µs | 535µs | 15381 | 1957.0 | 25.2 MiB |
| typing-sparse | visible | 32 | 6400 | 111ms | 57700 | 330µs | 1.652ms | 2.591ms | 15381 | 1957.0 | 80.6 MiB |
| typing-sparse | changed | 1 | 200 | 1ms | 155462 | 6µs | 7µs | 9µs | 665 | 91.0 | 2.5 MiB |
| typing-sparse | changed | 10 | 2000 | 4ms | 447226 | 17µs | 37µs | 102µs | 665 | 91.0 | 25.1 MiB |
| typing-sparse | changed | 32 | 6400 | 13ms | 503553 | 28µs | 210µs | 513µs | 665 | 91.0 | 80.3 MiB |
| typing-sparse | snapshot | 1 | 200 | 1ms | 171526 | 6µs | 7µs | 9µs | 135 | 6.0 | 2.5 MiB |
| typing-sparse | snapshot | 10 | 2000 | 5ms | 429033 | 19µs | 42µs | 102µs | 135 | 6.0 | 25.3 MiB |
| typing-sparse | snapshot | 32 | 6400 | 15ms | 428845 | 38µs | 223µs | 667µs | 135 | 6.0 | 80.7 MiB |
| scroll-dense | feed-only | 1 | 200 | 4ms | 50522 | 1µs | 74µs | 109µs | 0 | 1.0 | 2.5 MiB |
| scroll-dense | feed-only | 10 | 2000 | 9ms | 228396 | 1µs | 88µs | 168µs | 0 | 1.0 | 25.5 MiB |
| scroll-dense | feed-only | 32 | 6400 | 12ms | 519645 | 2µs | 186µs | 578µs | 0 | 1.0 | 81.0 MiB |
| scroll-dense | visible | 1 | 200 | 25ms | 8143 | 110µs | 203µs | 304µs | 23061 | 3876.0 | 2.6 MiB |
| scroll-dense | visible | 10 | 2000 | 61ms | 32690 | 275µs | 528µs | 707µs | 23061 | 3876.0 | 25.7 MiB |
| scroll-dense | visible | 32 | 6400 | 162ms | 39475 | 507µs | 2.175ms | 3.083ms | 23061 | 3876.0 | 81.8 MiB |
| scroll-dense | changed | 1 | 200 | 26ms | 7754 | 120µs | 161µs | 338µs | 23157 | 3896.0 | 2.6 MiB |
| scroll-dense | changed | 10 | 2000 | 51ms | 39340 | 204µs | 454µs | 837µs | 23157 | 3896.0 | 25.8 MiB |
| scroll-dense | changed | 32 | 6400 | 146ms | 43797 | 487µs | 1.841ms | 2.754ms | 23157 | 3896.0 | 82.4 MiB |
| scroll-dense | snapshot | 1 | 200 | 14ms | 14616 | 75µs | 83µs | 120µs | 2528 | 5.0 | 2.6 MiB |
| scroll-dense | snapshot | 10 | 2000 | 19ms | 107080 | 81µs | 158µs | 258µs | 2528 | 5.0 | 26.1 MiB |
| scroll-dense | snapshot | 32 | 6400 | 42ms | 151366 | 169µs | 429µs | 699µs | 2528 | 5.0 | 83.5 MiB |
| max-geometry | feed-only | 1 | 200 | 50ms | 3966 | 237µs | 395µs | 416µs | 0 | 1.0 | 3.6 MiB |
| max-geometry | feed-only | 10 | 2000 | 85ms | 23646 | 393µs | 656µs | 906µs | 0 | 1.0 | 35.7 MiB |
| max-geometry | feed-only | 32 | 6400 | 189ms | 33782 | 781µs | 1.785ms | 2.377ms | 0 | 1.0 | 114.6 MiB |
| max-geometry | visible | 1 | 200 | 412ms | 485 | 1.989ms | 2.558ms | 2.779ms | 393237 | 65612.0 | 3.9 MiB |
| max-geometry | visible | 10 | 2000 | 751ms | 2661 | 3.577ms | 5.079ms | 5.665ms | 393237 | 65612.1 | 38.6 MiB |
| max-geometry | visible | 32 | 6400 | 1.563s | 4096 | 7.343ms | 11.971ms | 14.679ms | 393237 | 65612.0 | 123.6 MiB |
| max-geometry | changed | 1 | 200 | 398ms | 502 | 1.92ms | 2.532ms | 2.974ms | 393493 | 65672.0 | 3.9 MiB |
| max-geometry | changed | 10 | 2000 | 655ms | 3056 | 3.148ms | 4.317ms | 4.993ms | 393493 | 65672.1 | 38.8 MiB |
| max-geometry | changed | 32 | 6400 | 1.634s | 3918 | 7.666ms | 12.455ms | 14.94ms | 393493 | 65672.1 | 124.0 MiB |
| max-geometry | snapshot | 1 | 200 | 123ms | 1620 | 566µs | 935µs | 1.079ms | 34610 | 5.0 | 4.5 MiB |
| max-geometry | snapshot | 10 | 2000 | 210ms | 9524 | 955µs | 1.784ms | 2.016ms | 34610 | 5.0 | 45.5 MiB |
| max-geometry | snapshot | 32 | 6400 | 584ms | 10957 | 2.639ms | 4.937ms | 6.28ms | 34610 | 5.1 | 145.1 MiB |

## Benchmarks

`go test -run '^$' -bench 'Readout' -benchtime=2s`, total 196.147 s.
`ns/op` is wall time per *per-helper* event at that concurrency; divide the
batch throughput out of `events/s` instead if you want the aggregate. Each row
is a single sample — no `-count` repeats, so run-to-run variance is not
characterised.

```
goos: linux
goarch: amd64
pkg: github.com/self-deprecated/agent-reel/internal/castexport
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S          
BenchmarkReadout/redraw-unchanged/feed-only/helpers-1-32         	  133923	     17441 ns/op	         2.543 MiB/helper	         1.000 allocs/event	         0 bytes/event	     57338 events/s	     51748 p95-ns/event
BenchmarkReadout/redraw-unchanged/feed-only/helpers-10-32        	  108906	     23419 ns/op	         2.525 MiB/helper	         1.000 allocs/event	         0 bytes/event	    427001 events/s	     68790 p95-ns/event
BenchmarkReadout/redraw-unchanged/feed-only/helpers-32-32        	   37449	     60620 ns/op	         2.523 MiB/helper	         1.000 allocs/event	         0 bytes/event	    527867 events/s	    207631 p95-ns/event
BenchmarkReadout/redraw-unchanged/visible/helpers-1-32           	   18747	    123972 ns/op	         2.555 MiB/helper	      3876 allocs/event	     23061 bytes/event	      8066 events/s	    178697 p95-ns/event
BenchmarkReadout/redraw-unchanged/visible/helpers-10-32          	    8640	    283263 ns/op	         2.536 MiB/helper	      3876 allocs/event	     23061 bytes/event	     35302 events/s	    490634 p95-ns/event
BenchmarkReadout/redraw-unchanged/visible/helpers-32-32          	    2814	    723412 ns/op	         2.538 MiB/helper	      3876 allocs/event	     23061 bytes/event	     44234 events/s	   1892042 p95-ns/event
BenchmarkReadout/redraw-unchanged/changed/helpers-1-32           	   20041	    120187 ns/op	         2.535 MiB/helper	      3896 allocs/event	     23157 bytes/event	      8320 events/s	    173346 p95-ns/event
BenchmarkReadout/redraw-unchanged/changed/helpers-10-32          	    9062	    221236 ns/op	         2.538 MiB/helper	      3896 allocs/event	     23157 bytes/event	     45200 events/s	    405363 p95-ns/event
BenchmarkReadout/redraw-unchanged/changed/helpers-32-32          	    3642	    623272 ns/op	         2.540 MiB/helper	      3896 allocs/event	     23157 bytes/event	     51341 events/s	   1593099 p95-ns/event
BenchmarkReadout/redraw-unchanged/snapshot/helpers-1-32          	   47102	     51246 ns/op	         2.602 MiB/helper	         5.000 allocs/event	      2653 bytes/event	     19514 events/s	     72697 p95-ns/event
BenchmarkReadout/redraw-unchanged/snapshot/helpers-10-32         	   29484	     80316 ns/op	         2.590 MiB/helper	         5.000 allocs/event	      2653 bytes/event	    124504 events/s	    128061 p95-ns/event
BenchmarkReadout/redraw-unchanged/snapshot/helpers-32-32         	   12249	    203057 ns/op	         2.575 MiB/helper	         5.002 allocs/event	      2653 bytes/event	    157588 events/s	    435290 p95-ns/event
BenchmarkReadout/idle/feed-only/helpers-1-32                     	40176349	        55.65 ns/op	         2.516 MiB/helper	         0.0000001 allocs/event	         0 bytes/event	  17969408 events/s	        21.00 p95-ns/event
BenchmarkReadout/idle/feed-only/helpers-10-32                    	41439118	        59.08 ns/op	         2.509 MiB/helper	         0.0000001 allocs/event	         0 bytes/event	 169247253 events/s	        21.00 p95-ns/event
BenchmarkReadout/idle/feed-only/helpers-32-32                    	22875409	        98.11 ns/op	         2.512 MiB/helper	         0.0000001 allocs/event	         0 bytes/event	 326146895 events/s	        30.00 p95-ns/event
BenchmarkReadout/idle/visible/helpers-1-32                       	   25092	     92312 ns/op	         2.539 MiB/helper	      1955 allocs/event	     15381 bytes/event	     10832 events/s	    172355 p95-ns/event
BenchmarkReadout/idle/visible/helpers-10-32                      	   11456	    195600 ns/op	         2.513 MiB/helper	      1955 allocs/event	     15381 bytes/event	     51124 events/s	    409691 p95-ns/event
BenchmarkReadout/idle/visible/helpers-32-32                      	    4142	    532521 ns/op	         2.517 MiB/helper	      1955 allocs/event	     15381 bytes/event	     60091 events/s	   1764772 p95-ns/event
BenchmarkReadout/idle/changed/helpers-1-32                       	  842277	      3029 ns/op	         2.516 MiB/helper	         6.000 allocs/event	        21.00 bytes/event	    330143 events/s	      4198 p95-ns/event
BenchmarkReadout/idle/changed/helpers-10-32                      	  147469	     17070 ns/op	         2.513 MiB/helper	         6.000 allocs/event	        21.00 bytes/event	    585803 events/s	     38863 p95-ns/event
BenchmarkReadout/idle/changed/helpers-32-32                      	  111850	     20836 ns/op	         2.521 MiB/helper	         6.000 allocs/event	        21.00 bytes/event	   1535778 events/s	     55895 p95-ns/event
BenchmarkReadout/idle/snapshot/helpers-1-32                      	  510570	      4924 ns/op	         2.539 MiB/helper	         4.000 allocs/event	        26.00 bytes/event	    203083 events/s	      6442 p95-ns/event
BenchmarkReadout/idle/snapshot/helpers-10-32                     	  134259	     18312 ns/op	         2.523 MiB/helper	         4.000 allocs/event	        26.00 bytes/event	    546072 events/s	     35948 p95-ns/event
BenchmarkReadout/idle/snapshot/helpers-32-32                     	   67315	     37417 ns/op	         2.520 MiB/helper	         4.000 allocs/event	        26.00 bytes/event	    855220 events/s	    105268 p95-ns/event
BenchmarkReadout/typing-sparse/feed-only/helpers-1-32            	 5014651	       476.0 ns/op	         2.480 MiB/helper	         2.000 allocs/event	         0 bytes/event	   2100991 events/s	       791.0 p95-ns/event
BenchmarkReadout/typing-sparse/feed-only/helpers-10-32           	 3774264	       723.3 ns/op	         2.522 MiB/helper	         2.000 allocs/event	         0 bytes/event	  13824570 events/s	      1242 p95-ns/event
BenchmarkReadout/typing-sparse/feed-only/helpers-32-32           	 1557967	      1855 ns/op	         2.526 MiB/helper	         2.000 allocs/event	         0 bytes/event	  17252349 events/s	      6412 p95-ns/event
BenchmarkReadout/typing-sparse/visible/helpers-1-32              	   26899	     87037 ns/op	         2.527 MiB/helper	      1957 allocs/event	     15381 bytes/event	     11489 events/s	    169800 p95-ns/event
BenchmarkReadout/typing-sparse/visible/helpers-10-32             	   11822	    194994 ns/op	         2.515 MiB/helper	      1957 allocs/event	     15381 bytes/event	     51284 events/s	    429068 p95-ns/event
BenchmarkReadout/typing-sparse/visible/helpers-32-32             	    4095	    524973 ns/op	         2.520 MiB/helper	      1957 allocs/event	     15381 bytes/event	     60955 events/s	   1694941 p95-ns/event
BenchmarkReadout/typing-sparse/changed/helpers-1-32              	  270091	      8227 ns/op	         2.531 MiB/helper	       114.4 allocs/event	       848.9 bytes/event	    121549 events/s	     10600 p95-ns/event
BenchmarkReadout/typing-sparse/changed/helpers-10-32             	  105430	     20765 ns/op	         2.535 MiB/helper	       114.1 allocs/event	       846.8 bytes/event	    481584 events/s	     35918 p95-ns/event
BenchmarkReadout/typing-sparse/changed/helpers-32-32             	   40965	     51868 ns/op	         2.536 MiB/helper	       113.5 allocs/event	       841.8 bytes/event	    616950 events/s	    168487 p95-ns/event
BenchmarkReadout/typing-sparse/snapshot/helpers-1-32             	   98095	     25869 ns/op	         2.582 MiB/helper	         6.000 allocs/event	      1896 bytes/event	     38657 events/s	     33893 p95-ns/event
BenchmarkReadout/typing-sparse/snapshot/helpers-10-32            	   59538	     40368 ns/op	         2.595 MiB/helper	         6.000 allocs/event	      1885 bytes/event	    247723 events/s	     67046 p95-ns/event
BenchmarkReadout/typing-sparse/snapshot/helpers-32-32            	   24481	    102368 ns/op	         2.576 MiB/helper	         6.001 allocs/event	      1842 bytes/event	    312597 events/s	    236866 p95-ns/event
BenchmarkReadout/scroll-dense/feed-only/helpers-1-32             	  127341	     18898 ns/op	         2.535 MiB/helper	         1.000 allocs/event	         0 bytes/event	     52915 events/s	     67136 p95-ns/event
BenchmarkReadout/scroll-dense/feed-only/helpers-10-32            	  100822	     24872 ns/op	         2.521 MiB/helper	         1.000 allocs/event	         0 bytes/event	    402058 events/s	     73248 p95-ns/event
BenchmarkReadout/scroll-dense/feed-only/helpers-32-32            	   37646	     69669 ns/op	         2.541 MiB/helper	         1.000 allocs/event	         0 bytes/event	    459306 events/s	    242747 p95-ns/event
BenchmarkReadout/scroll-dense/visible/helpers-1-32               	   16606	    146916 ns/op	         2.562 MiB/helper	      3876 allocs/event	     23061 bytes/event	      6806 events/s	    253697 p95-ns/event
BenchmarkReadout/scroll-dense/visible/helpers-10-32              	    7947	    281989 ns/op	         2.564 MiB/helper	      3876 allocs/event	     23061 bytes/event	     35461 events/s	    548022 p95-ns/event
BenchmarkReadout/scroll-dense/visible/helpers-32-32              	    2834	    764995 ns/op	         2.567 MiB/helper	      3876 allocs/event	     23061 bytes/event	     41829 events/s	   2011237 p95-ns/event
BenchmarkReadout/scroll-dense/changed/helpers-1-32               	   18788	    125672 ns/op	         2.590 MiB/helper	      3896 allocs/event	     23157 bytes/event	      7957 events/s	    182594 p95-ns/event
BenchmarkReadout/scroll-dense/changed/helpers-10-32              	    9566	    235147 ns/op	         2.563 MiB/helper	      3896 allocs/event	     23157 bytes/event	     42526 events/s	    479553 p95-ns/event
BenchmarkReadout/scroll-dense/changed/helpers-32-32              	    3600	    625841 ns/op	         2.565 MiB/helper	      3896 allocs/event	     23157 bytes/event	     51130 events/s	   1677128 p95-ns/event
BenchmarkReadout/scroll-dense/snapshot/helpers-1-32              	   46028	     51379 ns/op	         2.625 MiB/helper	         5.000 allocs/event	      2528 bytes/event	     19463 events/s	     72627 p95-ns/event
BenchmarkReadout/scroll-dense/snapshot/helpers-10-32             	   27933	     81226 ns/op	         2.605 MiB/helper	         5.000 allocs/event	      2528 bytes/event	    123114 events/s	    128252 p95-ns/event
BenchmarkReadout/scroll-dense/snapshot/helpers-32-32             	   10000	    207882 ns/op	         2.602 MiB/helper	         5.002 allocs/event	      2528 bytes/event	    153931 events/s	    453514 p95-ns/event
BenchmarkReadout/max-geometry/feed-only/helpers-1-32             	   10000	    239124 ns/op	         3.594 MiB/helper	         1.001 allocs/event	         0 bytes/event	      4182 events/s	    272513 p95-ns/event
BenchmarkReadout/max-geometry/feed-only/helpers-10-32            	    6499	    385382 ns/op	         3.583 MiB/helper	         1.000 allocs/event	         0 bytes/event	     25948 events/s	    534496 p95-ns/event
BenchmarkReadout/max-geometry/feed-only/helpers-32-32            	    2769	    942906 ns/op	         3.577 MiB/helper	         1.001 allocs/event	         0 bytes/event	     33937 events/s	   1824415 p95-ns/event
BenchmarkReadout/max-geometry/visible/helpers-1-32               	    1114	   2184417 ns/op	         3.875 MiB/helper	     65612 allocs/event	    393237 bytes/event	       457.8 events/s	   2870835 p95-ns/event
BenchmarkReadout/max-geometry/visible/helpers-10-32              	     602	   4075858 ns/op	         3.871 MiB/helper	     65612 allocs/event	    393237 bytes/event	      2453 events/s	   5714280 p95-ns/event
BenchmarkReadout/max-geometry/visible/helpers-32-32              	     295	   8192755 ns/op	         3.874 MiB/helper	     65612 allocs/event	    393237 bytes/event	      3906 events/s	  13238718 p95-ns/event
BenchmarkReadout/max-geometry/changed/helpers-1-32               	    1124	   2367147 ns/op	         3.875 MiB/helper	     65672 allocs/event	    393493 bytes/event	       422.4 events/s	   4026281 p95-ns/event
BenchmarkReadout/max-geometry/changed/helpers-10-32              	     278	   9496928 ns/op	         3.862 MiB/helper	     65672 allocs/event	    393493 bytes/event	      1053 events/s	  17145273 p95-ns/event
BenchmarkReadout/max-geometry/changed/helpers-32-32              	     282	   8040486 ns/op	         3.872 MiB/helper	     65672 allocs/event	    393493 bytes/event	      3980 events/s	  12270414 p95-ns/event
BenchmarkReadout/max-geometry/snapshot/helpers-1-32              	    3817	    611397 ns/op	         4.539 MiB/helper	         5.002 allocs/event	     34610 bytes/event	      1636 events/s	    901929 p95-ns/event
BenchmarkReadout/max-geometry/snapshot/helpers-10-32             	    2265	   1055675 ns/op	         4.534 MiB/helper	         5.002 allocs/event	     34610 bytes/event	      9473 events/s	   1876574 p95-ns/event
BenchmarkReadout/max-geometry/snapshot/helpers-32-32             	     830	   3121063 ns/op	         4.543 MiB/helper	         5.044 allocs/event	     34610 bytes/event	     10253 events/s	   5615213 p95-ns/event
PASS
ok  	github.com/self-deprecated/agent-reel/internal/castexport	196.147s
```

## What the numbers show

Only what is in the tables above. No budget follows from any of it.

**`changed` is cheaper than `visible` exactly in proportion to how few rows are
dirty, and not at all otherwise.** At 80x24 with one character typed per event,
`changed` moved 665 bytes and 91 allocations per event against `visible`'s
15,381 bytes and 1,955 allocations — 23x fewer bytes, 21x fewer allocations, and
p50 6 µs against 72 µs. On an idle screen it is 21 bytes against 15,381. But
against `redraw-unchanged` the two are indistinguishable (23,157 vs 23,061 bytes
per event; 107 µs vs 105 µs p50 at one helper), because an identical repaint
dirties every row: `changed` then carries the whole screen *plus* four bytes of
row index each. `scroll-dense` behaves the same way. This is the contract's
"dirty rows are hints, not a verdict" showing up as cost: a `Changed()` on a
screen that did not visibly change is priced like a full readout.

**`snapshot` is cheap for Go and is not a substitute.** The legacy continuation
dump costs 4–6 allocations per event at every workload and geometry, because Go
receives one opaque string and decodes no cells: 2,653 bytes and 46 µs at
80x24 redraw against 23,061 bytes and 105 µs for `visible`, and at max-geometry
34,610 bytes and 566 µs against 393,237 bytes and 1.99 ms. That gap is the price
of the canonical model — per-cell structure that can be compared — not waste to
be recovered. `Checkpoint` answers a different question (see
`visual-recording-readout.md`) and cannot be compared cell-wise at all.

**Per-cell decode dominates, and scales with the cell count and the pen.**
`visible` cost tracks cells exactly, with 21 bytes of framing on top: an
unpainted 80x24 screen is 1,920 default-pen cells at 8 bytes → 15,381 bytes and
1,955 allocations; the same screen painted with an indexed foreground and an RGB
background is 12 bytes per cell → 23,061 bytes and 3,876 allocations; 512x64
painted is 32,768 cells → 393,237 bytes and 65,612 allocations. So the wire cost
is 8–14 bytes per cell depending on the pen, and the allocation cost is one per
cell plus one more per RGB colour — which is what `io.ReadFull` on a local array
in `decodeCell`/`decodeColor` would produce. That is an observation about where
the allocations sit, not a change proposal; `screen.go` is untouched. The feed
side is negligible by comparison (1–2 allocations per event at every workload),
so readout cost is the readout, not the emulation.

**1 → 32 helpers buys roughly 4–5x aggregate throughput, at 4–7x the per-event
latency.** For `redraw-unchanged`/`visible`: 8,541 → 40,918 events/s (4.8x)
while p50 goes 105 µs → 464 µs and p99 260 µs → 3.11 ms. `max-geometry`/`visible`:
485 → 4,096 events/s (8.4x) with p50 1.99 ms → 7.34 ms. Most of the gain is
already taken by 10 helpers (e.g. 32,336 events/s, 79% of the 32-helper figure,
for `redraw-unchanged`/`visible`). Tail latency degrades faster than median
throughput improves in every mode.

**Helper memory is small and flat in the event count.** ~2.5 MiB resident per
80x24 helper regardless of workload or how many events it has processed, rising
to 3.6–4.5 MiB at 512x64. 32 helpers cost 80–83 MiB at 80x24 and 115–145 MiB at
512x64. Nothing grew over a batch.

## Caveats

- **One host, one run, no repeats.** No `-count`, no statistical treatment. One
  benchmark cell is visibly an outlier — `max-geometry/changed/helpers-10`
  reported 9.50 ms/op and 1,053 events/s, against 8.04 ms at 32 helpers and
  3.15 ms p50 in the 200-event matrix. Treat differences of that size between
  cells as noise unless they are reproduced.
- **`feed-only` does not drain the helper.** It writes and never reads, so when
  the batch clock stops up to one pipe buffer (64 KiB) of fed bytes may still be
  unprocessed. At `max-geometry`, whose payload is ~34 kB per event, that is
  under two events of the 200 (<1%); at 80x24 it is smaller still. It is a
  writer-side baseline, not a measure of emulation throughput.
- **`idle`/`feed-only` is a degenerate cell.** No feed and no readout: it
  measures the harness loop and nothing else, and its "events/s" of tens of
  millions should be read as the noise floor of the measurement, not as work.
- **`typing-sparse` results depend on the event count**, because the screen
  fills as characters accumulate. The 200-event matrix shows `snapshot` at 135
  bytes per event; the benchmark, running tens of thousands of events, shows
  ~1.9 kB. Both are correct for their run length.
- **`allocs/event` is process-global.** It is a `Mallocs` delta over the whole
  batch, so it charges the harness's own per-event bookkeeping to the readout.
  It is a comparison between modes, not an exact decoder allocation count.
- **The `-race` figures are not recorded here.** `go test -race -run
  '^TestReadoutMeasurements$'` passes (48.9 s against 8.9 s without), but the
  race detector changes both timing and allocation counts, so none of its
  numbers appear above.
- **Nothing here is a budget.** No coalescing, sampling or capture-rate decision
  is made or implied. Browser rendering, screenshot capture, encoding and any
  parity comparison are outside this measurement entirely.
