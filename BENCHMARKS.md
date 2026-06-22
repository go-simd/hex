# Performance parity — go-simd/hex vs stdlib / reference

**Methodology.** Apple M4 Max (arm64, NEON), macOS (Darwin 25.5.0), Go 1.26.4,
single core. References: `encoding/hex` (Go stdlib — also the scalar fallback of
go-simd), `github.com/tmthrgd/go-hex` (pure-Go SIMD hex). Inputs: pseudo-random
bytes (seed 2), sizes 64 B / 1 KiB / 16 KiB / 1 MiB; `-benchtime=0.3s -count=3`,
median reported. Throughput is over the **source** domain (raw bytes for encode,
hex text for decode). Correctness: `go test` round-trips and byte-matches
`encoding/hex` on every size. Reproduce:

```
GOWORK=off go test -run='^$' -bench=Parity -benchmem -benchtime=0.3s -count=3 .
```

> **arm64 caveat (this host).** go-simd/hex ships SIMD kernels for **amd64,
> ppc64le and s390x only** — there is *no* arm64/NEON kernel yet. On this host
> `go-simd/hex` therefore *is* `encoding/hex` (zero-overhead fallback), which the
> numbers below confirm (gosimd ≈ stdlib to within noise). The real SIMD speedup
> for hex must be measured on **amd64/AVX2** (follow-up — needs an x86_64 host).

## Encode

| op | size | go-simd (GB/s) | stdlib | tmthrgd (SIMD ref) | ratio vs stdlib | ratio vs tmthrgd | verdict |
|----|------|---------------:|-------:|-------------------:|----------------:|-----------------:|---------|
| encode | 64 B   | 1.84 | 1.84 | 1.84 | 1.00× | 1.00× | arm64 fallback = stdlib |
| encode | 1 KiB  | 1.78 | 1.83 | 1.82 | 0.98× | 0.98× | arm64 fallback = stdlib |
| encode | 16 KiB | 1.90 | 1.94 | 1.89 | 0.98× | 1.00× | arm64 fallback = stdlib |
| encode | 1 MiB  | 1.93 | 1.94 | 1.91 | 1.00× | 1.01× | arm64 fallback = stdlib |

## Decode

| op | size | go-simd (GB/s) | stdlib | tmthrgd (SIMD ref) | ratio vs stdlib | ratio vs tmthrgd | verdict |
|----|------|---------------:|-------:|-------------------:|----------------:|-----------------:|---------|
| decode | 64 B   | 3.32 | 3.33 | 1.31 | 1.00× | 2.53× | =stdlib; beats tmthrgd arm64 |
| decode | 1 KiB  | 3.38 | 3.39 | 1.15 | 1.00× | 2.93× | =stdlib; beats tmthrgd arm64 |
| decode | 16 KiB | 3.47 | 3.48 | 1.12 | 1.00× | 3.10× | =stdlib; beats tmthrgd arm64 |
| decode | 1 MiB  | 3.46 | 3.47 | 0.36 | 1.00× | **9.7×** | =stdlib; tmthrgd arm64 collapses |

## Summary

* On **arm64 this is a stdlib fallback**, so go-simd ≈ stdlib by construction
  (encode 0.98–1.00×, decode 1.00×). This *confirms the fallback is
  zero-overhead* — no regression vs stdlib — but it is **not** a SIMD result.
* Notable side effect: because the fallback is stdlib, go-simd **beats the
  tmthrgd "SIMD" reference on arm64** (2.5–9.7× on decode) — tmthrgd's arm64
  path is much slower than Go's own stdlib hex. So go-simd is already the better
  choice on arm64, just not via its own SIMD.

### Action items
1. **Add an arm64/NEON hex kernel** (encode: nibble-split + table lookup;
   decode: validate + nibble-pack). This is the main gap — utf8 has the same
   hole. go-asmgen already targets arm64 for base64/matchlen, so the kernel
   shape is available.
2. **amd64/AVX2 follow-up:** run this harness on a real x86_64 VM to quantify the
   actual SIMD speedup vs stdlib and tmthrgd there (Rosetta = no AVX2).
