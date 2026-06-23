# Performance parity — go-simd/hex vs stdlib / reference

**Methodology.** Apple M4 Max (arm64, NEON), macOS (Darwin 25.5.0), Go 1.26.4,
single core. References: `encoding/hex` (Go stdlib — also the scalar fallback of
go-simd), `github.com/tmthrgd/go-hex` (pure-Go SIMD hex). Inputs: pseudo-random
bytes (seed 2), sizes 64 B / 1 KiB / 16 KiB / 1 MiB; `-benchtime=0.3s -count=3`,
median reported. Throughput is over the **source** domain (raw bytes for encode,
hex text for decode). Correctness: `go test` round-trips and byte-matches
`encoding/hex` on every size, plus a fuzz-validated every-offset invalid-byte
sweep through the NEON decode kernel. Reproduce:

```
GOWORK=off go test -run='^$' -bench=Parity -benchmem -benchtime=0.3s -count=3 .
```

> **arm64 NEON kernel (this host).** As of 2026-06-22 go-simd/hex ships a real
> **arm64/NEON** kernel (encode: nibble split + 16-byte VTBL lookup + VST2
> interleaving store; decode: VLD2 deinterleaving load + range-validate + nibble
> fuse), alongside the existing amd64/ppc64le/s390x kernels. The numbers below
> are measured natively on this NEON host — they are a SIMD result, no longer a
> stdlib fallback.

## Encode

| op | size | go-simd (GB/s) | stdlib | tmthrgd (SIMD ref) | ratio vs stdlib | ratio vs tmthrgd | verdict |
|----|------|---------------:|-------:|-------------------:|----------------:|-----------------:|---------|
| encode | 64 B   | 11.22 | 1.75 | 1.82 |  6.4× |  6.2× | NEON wins |
| encode | 1 KiB  | 29.36 | 1.79 | 1.75 | 16.4× | 16.8× | NEON wins |
| encode | 16 KiB | 35.62 | 1.92 | 1.87 | 18.6× | 19.0× | NEON wins |
| encode | 1 MiB  | 40.19 | 1.88 | 1.75 | 21.4× | 23.0× | NEON wins |

## Decode

| op | size | go-simd (GB/s) | stdlib | tmthrgd (SIMD ref) | ratio vs stdlib | ratio vs tmthrgd | verdict |
|----|------|---------------:|-------:|-------------------:|----------------:|-----------------:|---------|
| decode | 64 B   | 7.53 | 3.02 | 1.26 | 2.49× | 5.98× | NEON wins |
| decode | 1 KiB  | 7.85 | 3.25 | 1.15 | 2.42× | 6.83× | NEON wins |
| decode | 16 KiB | 8.34 | 3.37 | 1.08 | 2.47× | 7.74× | NEON wins |
| decode | 1 MiB  | 8.08 | 3.25 | 0.33 | 2.49× | **24.5×** | NEON wins |

## Before → after (arm64)

Prior to the NEON kernel, go-simd/hex *was* `encoding/hex` on arm64 (zero-overhead
stdlib fallback), so go-simd ≈ stdlib by construction:

| op (1 MiB) | before (×stdlib) | after (×stdlib) |
|------------|-----------------:|----------------:|
| encode | 1.00× (fallback) | **21.4×** |
| decode | 1.00× (fallback) | **2.49×** |

## amd64 (AVX2, GitHub Actions x86_64 runner — ratios valid, absolute ns/op CI-noisy)

**Methodology.** GitHub Actions `ubuntu-latest` runner, **AMD EPYC** (`avx2`
present, **no `avx512*`** — confirmed from `/proc/cpuinfo`), `GOAMD64` baseline,
Go stable, single core. Same parity harness, `-count=6`, **min-of-6**. The runner
is shared, so absolute throughput is noisy and **not comparable to the arm64 M4
Max rows above** (different hardware/ISA); the **ratios** (ours/stdlib,
ours/tmthrgd) are measured back-to-back on the *same* CPU and are valid.
Reproduce via `gh workflow run bench-amd64.yml`.

### Encode (amd64 AVX2)

| size | go-simd (MB/s) | stdlib | tmthrgd (SIMD ref) | ×stdlib | ×tmthrgd | verdict |
|------|---------------:|-------:|-------------------:|--------:|---------:|---------|
| 64 B   |  6987 | 936 |  8310 |  7.46× | 0.84× | beats stdlib; ~0.84× SIMD ref (tiny) |
| 1 KiB  | 26818 | 932 | 18805 | 28.79× | 1.43× | **beats SIMD ref** |
| 16 KiB | 27631 | 932 | 19811 | 29.65× | 1.39× | **beats SIMD ref** |
| 1 MiB  | 22098 | 929 | 19756 | 23.78× | 1.12× | **beats SIMD ref** |

### Decode (amd64 AVX2)

| size | go-simd (MB/s) | stdlib | tmthrgd (SIMD ref) | ×stdlib | ×tmthrgd | verdict |
|------|---------------:|-------:|-------------------:|--------:|---------:|---------|
| 64 B   |  7573 | 1837 | 6968 | 4.12× | 1.09× | wins both |
| 1 KiB  | 12525 | 1873 | 8666 | 6.69× | 1.45× | wins both |
| 16 KiB | 13024 | 1892 | 8676 | 6.88× | 1.50× | wins both |
| 1 MiB  | 12950 | 1887 | 8646 | 6.86× | 1.50× | **wins both** |

* **Encode** beats stdlib up to **~30×** and **beats the tmthrgd SIMD reference**
  at every size ≥ 1 KiB (1.12–1.43×); only the 64 B tiny case trails it (0.84×).
* **Decode wins on both axes at every size**: ~4–7× stdlib and **1.09–1.50×
  tmthrgd**. Byte-/error-identical to `encoding/hex` (per-block rejection).

## Summary

* The new **arm64/NEON kernels win decisively** on both directions: encode is
  6–21× stdlib (and beats the tmthrgd SIMD reference 6–23×); decode is ~2.5×
  stdlib (and 6–24× tmthrgd, whose arm64 path collapses at 1 MiB).
* Output is byte- and error-identical to `encoding/hex` on every input,
  including invalid bytes at every offset and across block boundaries (the
  per-block rejection hands the exact tail to the scalar loop, so
  `InvalidByteError` offsets and `ErrLength` match stdlib bit-for-bit). 100%
  test coverage; fuzz-clean.

### Action items
1. ~~Add an arm64/NEON hex kernel.~~ **Done** (this revision).
2. ~~**amd64/AVX2 follow-up:** quantify the SIMD speedup vs stdlib and tmthrgd.~~
   **Done** (see the amd64 section) — measured on the GitHub Actions x86_64
   runner (EPYC, AVX2). Encode ~24–30× stdlib and **beats tmthrgd** ≥1 KiB;
   decode ~4–7× stdlib and **beats tmthrgd at every size** (1.09–1.50×).
