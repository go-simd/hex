package hex

// Standardized performance-parity harness: go-simd/hex (SIMD dispatch) vs
// encoding/hex (stdlib) vs github.com/tmthrgd/go-hex (pure-Go SIMD reference).
//
// NOTE: go-simd/hex ships SIMD kernels for amd64 / ppc64le / s390x only. On
// arm64 (this host) there is no NEON kernel yet, so the dispatch falls back to
// encoding/hex and go-simd == stdlib by construction. The arm64 numbers here
// therefore confirm the zero-overhead fallback, not a SIMD speedup; the real
// SIMD parity must be measured on amd64/AVX2 (follow-up, needs an x86 host).
//
//	GOWORK=off go test -run=^$ -bench='Parity' -benchmem .

import (
	"encoding/hex"
	"math/rand"
	"testing"

	tmthrgd "github.com/tmthrgd/go-hex"
)

var paritySizes = []int{64, 1024, 16384, 1 << 20}

func paritySrc(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(2)).Read(b)
	return b
}

func sizeLabel(n int) string {
	switch n {
	case 64:
		return "64B"
	case 1024:
		return "1KiB"
	case 16384:
		return "16KiB"
	case 1 << 20:
		return "1MiB"
	}
	return "?"
}

func BenchmarkParityEncode(b *testing.B) {
	for _, n := range paritySizes {
		src := paritySrc(n)
		dst := make([]byte, hex.EncodedLen(n))
		b.Run(sizeLabel(n)+"/gosimd", func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				Encode(dst, src)
			}
		})
		b.Run(sizeLabel(n)+"/stdlib", func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				hex.Encode(dst, src)
			}
		})
		b.Run(sizeLabel(n)+"/tmthrgd", func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				tmthrgd.Encode(dst, src)
			}
		})
	}
}

func BenchmarkParityDecode(b *testing.B) {
	for _, n := range paritySizes {
		enc := []byte(hex.EncodeToString(paritySrc(n)))
		dst := make([]byte, hex.DecodedLen(len(enc)))
		b.Run(sizeLabel(n)+"/gosimd", func(b *testing.B) {
			b.SetBytes(int64(len(enc)))
			for i := 0; i < b.N; i++ {
				Decode(dst, enc)
			}
		})
		b.Run(sizeLabel(n)+"/stdlib", func(b *testing.B) {
			b.SetBytes(int64(len(enc)))
			for i := 0; i < b.N; i++ {
				hex.Decode(dst, enc)
			}
		})
		b.Run(sizeLabel(n)+"/tmthrgd", func(b *testing.B) {
			b.SetBytes(int64(len(enc)))
			for i := 0; i < b.N; i++ {
				tmthrgd.Decode(dst, enc)
			}
		})
	}
}
