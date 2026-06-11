//go:build amd64

package hex

import (
	"bytes"
	stdhex "encoding/hex"
	"testing"
)

// decodeForce drives a chosen decode kernel (SSE or AVX2) directly over whole
// blocks, finishing with the scalar tail, so both amd64 paths are validated
// even when the runtime CPU (or Rosetta) would not dispatch to one of them. It
// mirrors Decode's structure: kernel over whole blocks, then decodeScalar.
func decodeForce(dst, src []byte, avx2 bool) (int, error) {
	var dd, sd int
	if avx2 && len(src) >= 64 {
		b := len(src) / 64
		done := decodeBlocksAVX2(dst, src, b)
		dd, sd = done*32, done*64
	} else if len(src) >= 32 {
		b := len(src) / 32
		done := decodeBlocksSSE(dst, src, b)
		dd, sd = done*16, done*32
	}
	return decodeScalar(dst, src, dd, sd)
}

// TestDecodeForceKernels validates both the SSE and AVX2 decode kernels against
// encoding/hex over valid input of many sizes, plus invalid bytes injected at
// every offset (in hi and lo nibble position, across block boundaries). AVX2 is
// only exercised when the CPU supports it (the instructions would #UD
// otherwise).
func TestDecodeForceKernels(t *testing.T) {
	for _, avx2 := range []bool{false, true} {
		if avx2 && !hasAVX2 {
			continue
		}
		// Valid round-trips.
		for _, n := range sizes {
			src := randBytes(n, int64(n)*13+7)
			enc := []byte(stdhex.EncodeToString(src))
			dst := make([]byte, DecodedLen(len(enc)))
			gn, ge := decodeForce(dst, enc, avx2)
			if ge != nil {
				t.Fatalf("avx2=%v n=%d valid: unexpected err %v", avx2, n, ge)
			}
			if !bytes.Equal(dst[:gn], src) {
				t.Fatalf("avx2=%v n=%d valid: mismatch", avx2, n)
			}
		}
		// Invalid byte injected at every offset of a multi-block string, in
		// both nibble positions, with a variety of bad chars.
		base := []byte(stdhex.EncodeToString(randBytes(200, 4))) // 400 chars
		bad := []byte{'/', ':', '@', 'G', '`', 'g', ' ', 0x00, 0xff}
		for _, bc := range bad {
			for off := 0; off < len(base); off++ {
				corrupt := append([]byte(nil), base...)
				corrupt[off] = bc
				gd := make([]byte, DecodedLen(len(corrupt)))
				wd := make([]byte, stdhex.DecodedLen(len(corrupt)))
				gn, ge := decodeForce(gd, corrupt, avx2)
				wn, we := stdhex.Decode(wd, corrupt)
				if gn != wn || errStr(ge) != errStr(we) {
					t.Fatalf("avx2=%v bc=%q off=%d: got (%d,%v) want (%d,%v)", avx2, bc, off, gn, ge, wn, we)
				}
				if !bytes.Equal(gd[:gn], wd[:wn]) {
					t.Fatalf("avx2=%v bc=%q off=%d bytes: got=%x want=%x", avx2, bc, off, gd[:gn], wd[:wn])
				}
			}
		}
	}
}

func errStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func benchDecodeForce(b *testing.B, avx2 bool) {
	if avx2 && !hasAVX2 {
		b.Skip("no AVX2")
	}
	enc := []byte(stdhex.EncodeToString(randBytes(1<<20, 2)))
	dst := make([]byte, DecodedLen(len(enc)))
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decodeForce(dst, enc, avx2)
	}
}

func BenchmarkDecodeForceSSE(b *testing.B)  { benchDecodeForce(b, false) }
func BenchmarkDecodeForceAVX2(b *testing.B) { benchDecodeForce(b, true) }
