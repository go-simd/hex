//go:build ppc64le

package hex

import (
	stdhex "encoding/hex"
	"math/rand"
	"testing"

	"golang.org/x/sys/cpu"
)

// TestDispatchPPC64LE drives both ppc64le dispatch branches for encode and
// decode: the VSX kernel and the scalar fallback. The kernels emit ISA-3.0
// (POWER9) instructions (LXVB16X/STXVB16X) that raise SIGILL on POWER8, so the
// kernel-forcing branch runs only when the host is actually POWER9+ (mirroring the
// amd64 force tests). The scalar-fallback branch (hasVSX=false) is always
// exercised and must not SIGILL on a POWER8 farm node. The power9-targeted QEMU CI
// job and the native POWER9/POWER10 farm runs cover the kernel branch. Every case
// stays byte- and error-identical to encoding/hex.
func TestDispatchPPC64LE(t *testing.T) {
	saved := hasVSX
	defer func() { hasVSX = saved }()

	rng := rand.New(rand.NewSource(17))
	check := func(tag string) {
		t.Helper()
		// Round-trip a spread of lengths through the public API.
		for _, n := range []int{0, 1, 7, 8, 15, 16, 17, 31, 32, 33, 48, 100, 1000, 4096} {
			src := make([]byte, n)
			rng.Read(src)
			enc := EncodeToString(src)
			if want := stdhex.EncodeToString(src); enc != want {
				t.Fatalf("%s encode n=%d:\n got=%q\nwant=%q", tag, n, enc, want)
			}
			gotB, err := DecodeString(enc)
			if err != nil || string(gotB) != string(src) {
				t.Fatalf("%s decode n=%d: err=%v round-trip mismatch", tag, n, err)
			}
		}
		// Invalid chars / odd lengths: error offset must match the stdlib exactly.
		for trial := 0; trial < 2000; trial++ {
			n := rng.Intn(80)
			src := make([]byte, n)
			rng.Read(src)
			enc := []byte(stdhex.EncodeToString(src))
			if len(enc) > 0 && rng.Intn(2) == 0 {
				enc[rng.Intn(len(enc))] = "!@#$ \tGZxy"[rng.Intn(10)]
			}
			if rng.Intn(4) == 0 && len(enc) > 0 {
				enc = enc[:len(enc)-1] // force an odd length sometimes
			}
			gotB, gotErr := DecodeString(string(enc))
			wantB, wantErr := stdhex.DecodeString(string(enc))
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("%s %q: err mismatch got=%v want=%v", tag, enc, gotErr, wantErr)
			}
			if gotErr != nil {
				if gotErr.Error() != wantErr.Error() {
					t.Fatalf("%s %q: err mismatch got=%v want=%v", tag, enc, gotErr, wantErr)
				}
				continue
			}
			if string(gotB) != string(wantB) {
				t.Fatalf("%s %q: decode mismatch", tag, enc)
			}
		}
	}

	// Scalar fallback: always safe on every ppc64le host (no VSX instructions).
	hasVSX = false
	check("fallback")

	// VSX kernel: force it on only when the CPU is POWER9+, otherwise the
	// LXVB16X/STXVB16X in the kernel would SIGILL (e.g. on a POWER8 farm node).
	if !cpu.PPC64.IsPOWER9 {
		t.Log("CPU is pre-POWER9; VSX kernel branch not exercised on this host")
		return
	}
	hasVSX = true
	check("vsx")
}
