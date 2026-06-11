//go:build amd64

package hex

import (
	stdhex "encoding/hex"
	"testing"
)

// encodeForce drives a chosen kernel (SSE or AVX2) directly over whole blocks,
// finishing the tail with the standard library, so both amd64 paths are tested
// even when the runtime CPU (or Rosetta) would not dispatch to one of them.
func encodeForce(dst, src []byte, avx2 bool) {
	n := len(src)
	if avx2 && n >= 32 {
		b := n / 32
		encodeBlocksAVX2(dst, src, b)
		stdhex.Encode(dst[b*64:], src[b*32:])
		return
	}
	if n >= 16 {
		b := n / 16
		encodeBlocksSSE(dst, src, b)
		stdhex.Encode(dst[b*32:], src[b*16:])
		return
	}
	stdhex.Encode(dst, src)
}

// TestEncodeForceKernels validates both the SSE and AVX2 kernels against
// encoding/hex over a range of sizes. AVX2 is only exercised when the CPU
// supports it (the instructions would #UD otherwise).
func TestEncodeForceKernels(t *testing.T) {
	for _, avx2 := range []bool{false, true} {
		if avx2 && !hasAVX2 {
			continue
		}
		for _, n := range sizes {
			src := randBytes(n, int64(n)*11+5)
			dst := make([]byte, EncodedLen(len(src)))
			encodeForce(dst, src, avx2)
			if want := stdhex.EncodeToString(src); string(dst) != want {
				t.Fatalf("avx2=%v n=%d:\n got=%q\nwant=%q", avx2, n, dst, want)
			}
		}
	}
}

func benchForce(b *testing.B, avx2 bool) {
	if avx2 && !hasAVX2 {
		b.Skip("no AVX2")
	}
	src := randBytes(1<<20, 2)
	dst := make([]byte, EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encodeForce(dst, src, avx2)
	}
}

func BenchmarkEncodeForceSSE(b *testing.B)  { benchForce(b, false) }
func BenchmarkEncodeForceAVX2(b *testing.B) { benchForce(b, true) }

// TestDispatchBranchesAMD64 drives every branch of the amd64 encodeSIMD and
// decodeSIMD dispatchers through the public Encode/Decode API. CI runs on a
// native AVX2 box where hasAVX2 is always true, so the SSE and scalar-only
// branches are only reachable by forcing hasAVX2 low (restored via defer).
func TestDispatchBranchesAMD64(t *testing.T) {
	check := func(n int) {
		src := randBytes(n, int64(n)*17+3)
		// Encode round-trips through encodeSIMD's dispatch.
		if got, want := EncodeToString(src), stdhex.EncodeToString(src); got != want {
			t.Fatalf("encode hasAVX2=%v n=%d:\n got=%q\nwant=%q", hasAVX2, n, got, want)
		}
		// Decode round-trips through decodeSIMD's dispatch.
		enc := stdhex.EncodeToString(src)
		dec, err := DecodeString(enc)
		if err != nil {
			t.Fatalf("decode hasAVX2=%v n=%d: %v", hasAVX2, n, err)
		}
		if string(dec) != string(src) {
			t.Fatalf("decode hasAVX2=%v n=%d: round-trip mismatch", hasAVX2, n)
		}
	}
	ns := []int{0, 8, 15, 16, 31, 32, 33, 63, 64, 65, 128}
	// Real CPU flag: exercises the AVX2 branch (n>=32 enc / n>=64 dec) and small n.
	for _, n := range ns {
		check(n)
	}
	// Forced low: exercises the SSE branch and the scalar-only return.
	saved := hasAVX2
	defer func() { hasAVX2 = saved }()
	hasAVX2 = false
	for _, n := range ns {
		check(n)
	}
}
