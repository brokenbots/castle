# Performance Baseline — Phase 1.3

**Status:** Baseline measurement (not a gate). These numbers are a fresh reference point after the reliability + conformance harness work in Phase 1.3.

**Measurement Date:** 2026-04-24
**Environment:** macOS (M-series), local single-machine setup (Castle + Overseer on localhost)

## Test Scenario

- **Workflow:** single shell step emitting 1000 StepLog chunks (`for i in $(seq 1 1000); do echo Log line $i: perf-sample; done`)
- **Runs:** 10 sequential runs
- **Codec:** proto
- **Transport:** Connect/gRPC over h2c (plaintext)

## Results

### Round-trip Latency (Per Run)

| Run | Time (ms) | Events |
|---|---:|---:|
| 1 | 214 | 1005 |
| 2 | 219 | 1005 |
| 3 | 209 | 1005 |
| 4 | 220 | 1005 |
| 5 | 211 | 1005 |
| 6 | 212 | 1005 |
| 7 | 210 | 1005 |
| 8 | 209 | 1005 |
| 9 | 212 | 1005 |
| 10 | 210 | 1005 |

### Percentiles

- **p50:** 211 ms
- **p95:** 220 ms
- **p99:** 220 ms

### Castle CPU Sampling (`ps -p <pid> -o %cpu` during each run)

- **Average peak per run:** 101.64%
- **Max observed peak:** 111.30%

### Binary and Bundle Sizes

- **Castle binary:** 21,987,298 bytes (~21.0 MB)
- **Overseer binary:** 20,265,506 bytes (~19.3 MB)
- **Parapet JS bundle (`dist/assets/index-*.js`):** 335,461 bytes
- **Parapet CSS bundle (`dist/assets/index-*.css`):** 8,486 bytes
- **Parapet dist total:** 344 KiB

## Delta vs 1.1

| Metric | Phase 1.1 | Phase 1.3 | Delta |
|---|---:|---:|---:|
| p50 latency | 201 ms | 211 ms | +10 ms (+5.0%) |
| p95 latency | 223 ms | 220 ms | -3 ms (-1.3%) |
| p99 latency | 226 ms | 220 ms | -6 ms (-2.7%) |
| Castle binary | 21 MB | ~21.0 MB | ~flat |
| Parapet bundle | 344 KB | 344 KiB | ~flat |

**Regression gate check:** p95 did not grow by >20%, so there is no perf blocker for archive close-out.

## Interpretation

- Latency distribution is broadly stable versus 1.1, with a small p50 increase but improved p95/p99.
- Binary and frontend artifact sizes are effectively unchanged.
- CPU peaks are expected for local bursty event ingestion and remained bounded in this baseline.

## Attribution Notes (1.3 Components)

Likely contributors to minor shape changes in latency/CPU profile:

- **Outbound buffer path:** additional in-memory replay bookkeeping for live-reconnect behavior.
- **Persistent cursor writes:** coalesced subscriber cursor persistence introduces small extra write work.
- **Reattach / crash-recovery path:** additional resume-related control/event plumbing in run lifecycle handling.

These are baseline observations, not optimization conclusions.

## Caveats

- Single-machine localhost baseline only; no network RTT or packet-loss effects.
- Sequential runs only; this does not characterize high concurrency.
- Baseline, not a gate: use this for trend comparison, not as a release blocker by itself.
