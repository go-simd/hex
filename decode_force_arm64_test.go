//go:build arm64

package hex

import (
	"bytes"
	stdhex "encoding/hex"
	"testing"
)

// decodeForce drives the NEON decode kernel directly over whole 32-char blocks,
// finishing with the scalar tail, so the arm64 path is validated exactly as
// Decode runs it: kernel over whole blocks, then decodeScalar.
func decodeForce(dst, src []byte) (int, error) {
	var dd, sd int
	if len(src) >= 32 {
		b := len(src) / 32
		done := decodeBlocks(dst, src, b)
		dd, sd = done*16, done*32
	}
	return decodeScalar(dst, src, dd, sd)
}

// TestDecodeForceKernelARM64 validates the NEON decode kernel against
// encoding/hex over valid input of many sizes, plus invalid bytes injected at
// every offset (in hi and lo nibble position, across block boundaries). This
// pins the per-block rejection (and thus the bit-exact InvalidByteError offset
// from the scalar tail) for the arm64 kernel.
func TestDecodeForceKernelARM64(t *testing.T) {
	// Valid round-trips.
	for _, n := range sizes {
		src := randBytes(n, int64(n)*13+7)
		enc := []byte(stdhex.EncodeToString(src))
		dst := make([]byte, DecodedLen(len(enc)))
		gn, ge := decodeForce(dst, enc)
		if ge != nil {
			t.Fatalf("n=%d valid: unexpected err %v", n, ge)
		}
		if !bytes.Equal(dst[:gn], src) {
			t.Fatalf("n=%d valid: mismatch", n)
		}
	}
	// Invalid byte injected at every offset of a multi-block string, in both
	// nibble positions, with a variety of bad chars (boundary chars just below/
	// above each accepted range, plus whitespace and the extreme bytes).
	base := []byte(stdhex.EncodeToString(randBytes(200, 4))) // 400 chars
	bad := []byte{'/', ':', '@', 'G', '`', 'g', ' ', 0x00, 0xff}
	for _, bc := range bad {
		for off := 0; off < len(base); off++ {
			corrupt := append([]byte(nil), base...)
			corrupt[off] = bc
			gd := make([]byte, DecodedLen(len(corrupt)))
			wd := make([]byte, stdhex.DecodedLen(len(corrupt)))
			gn, ge := decodeForce(gd, corrupt)
			wn, we := stdhex.Decode(wd, corrupt)
			if gn != wn || errStr(ge) != errStr(we) {
				t.Fatalf("bc=%q off=%d: got (%d,%v) want (%d,%v)", bc, off, gn, ge, wn, we)
			}
			if !bytes.Equal(gd[:gn], wd[:wn]) {
				t.Fatalf("bc=%q off=%d bytes: got=%x want=%x", bc, off, gd[:gn], wd[:wn])
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
