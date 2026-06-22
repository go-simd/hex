//go:build ignore

// Command gen produces decode_arm64.s with go-asmgen: a vectorised hex (base16)
// decoder for arm64 NEON, accepting the same alphabet as encoding/hex
// ('0'-'9','a'-'f','A'-'F'). Two input hex chars become one output byte; per 32
// input chars the kernel emits 16 bytes.
//
// Method:
//   - A deinterleaving load VLD2.P splits the 32 chars into two byte-planes:
//     V0 holds the high-nibble chars (input positions 0,2,4,...) and V1 the
//     low-nibble chars (positions 1,3,5,...). So each output byte's (hi,lo)
//     char pair is already separated, lane i = byte i.
//   - Validity per plane: a char is valid iff it lies in ['0','9'] OR ['A','F']
//     OR ['a','f']. Each range test is built from unsigned min/max (NEON has no
//     direct unsigned compare here): c>=lo iff VUMAX(c,lo)==c and c<=hi iff
//     VUMIN(c,hi)==c. The three range masks (0xFF valid / 0x00 not) are OR'd to
//     a per-char "valid" mask; the inverse is accumulated into a "bad" register
//     across both planes.
//   - Nibble value per plane: digit -> c-'0'; upper -> c-('A'-10); lower ->
//     c-('a'-10). Each subtraction is masked by its range mask and the three
//     OR'd, so valid chars yield their 0..15 nibble (invalid chars yield junk,
//     which is discarded because the block is rejected — see below).
//   - If the block contains any invalid char, the kernel STOPS and returns the
//     count of fully-decoded blocks; the Go wrapper then resumes a scalar
//     encoding/hex-style loop from there, so the reported InvalidByteError
//     offset is bit-exact and no SIMD pin-pointing is needed.
//   - Fuse: out = (hiNibble << 4) | loNibble (VSHL $4 then VORR), then VST1
//     stores the 16 bytes.
//
// Signature: decodeBlocks(dst, src []byte, n int) int — n = number of 32-char
// blocks to attempt; returns the number of blocks fully decoded before the
// first block containing an invalid char (== n when all blocks are valid).
//
// Run: go run decode_arm64_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/arm64"
	"github.com/go-asmgen/asmgen/emit"
)

func main() {
	f := emit.NewFile("arm64")

	sig := abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		[]abi.Arg{abi.Scalar("ret", abi.Int64)},
	)

	b := arm64.NewFunc("decodeBlocks", sig, 0)
	b.LoadArg("dst_base", "R0").
		LoadArg("src_base", "R1").
		LoadArg("n", "R2").
		// Splat the range bounds and nibble-bias constants once.
		Raw("MOVD $48, R4").Raw("VDUP R4, V20.B16").  // '0'
		Raw("MOVD $57, R4").Raw("VDUP R4, V21.B16").  // '9'
		Raw("MOVD $65, R4").Raw("VDUP R4, V22.B16").  // 'A'
		Raw("MOVD $70, R4").Raw("VDUP R4, V23.B16").  // 'F'
		Raw("MOVD $97, R4").Raw("VDUP R4, V24.B16").  // 'a'
		Raw("MOVD $102, R4").Raw("VDUP R4, V25.B16"). // 'f'
		Raw("MOVD $48, R4").Raw("VDUP R4, V26.B16").  // bias digit:  '0'
		Raw("MOVD $55, R4").Raw("VDUP R4, V27.B16").  // bias upper:  'A'-10
		Raw("MOVD $87, R4").Raw("VDUP R4, V28.B16").  // bias lower:  'a'-10
		Raw("MOVD $0, R3").                           // R3 = block counter
		Raw("CBZ R2, done").
		Label("loop").
		// Deinterleaving load: V0 = hi-nibble chars, V1 = lo-nibble chars.
		Raw("VLD2 (R1), [V0.B16, V1.B16]").
		Raw("VMOVI $255, V7.B16"). // V7 = good accumulator = all-ones
		Raw("")

	// classify decodes one char plane (register vc, e.g. "V0"): on return the
	// plane register holds the 0..15 nibble values, and V7 accumulates (ANDs in)
	// the per-char valid mask. Scratch: V2..V6, V16..V19.
	classify := func(vc string) {
		// isDigit = (c>=V20) & (c<=V21): VUMAX(c,'0')==c & VUMIN(c,'9')==c.
		b.Raw("VUMAX V20.B16, %s, V2.B16", vc).Raw("VCMEQ %s, V2.B16, V2.B16", vc)
		b.Raw("VUMIN V21.B16, %s, V3.B16", vc).Raw("VCMEQ %s, V3.B16, V3.B16", vc)
		b.Raw("VAND V3.B16, V2.B16, V16.B16") // V16 = isDigit
		// isUpper
		b.Raw("VUMAX V22.B16, %s, V2.B16", vc).Raw("VCMEQ %s, V2.B16, V2.B16", vc)
		b.Raw("VUMIN V23.B16, %s, V3.B16", vc).Raw("VCMEQ %s, V3.B16, V3.B16", vc)
		b.Raw("VAND V3.B16, V2.B16, V17.B16") // V17 = isUpper
		// isLower
		b.Raw("VUMAX V24.B16, %s, V2.B16", vc).Raw("VCMEQ %s, V2.B16, V2.B16", vc)
		b.Raw("VUMIN V25.B16, %s, V3.B16", vc).Raw("VCMEQ %s, V3.B16, V3.B16", vc)
		b.Raw("VAND V3.B16, V2.B16, V18.B16") // V18 = isLower
		// valid = isDigit | isUpper | isLower ; good &= valid (NEON lacks VMVN/
		// VBIC here, so we AND-accumulate validity and reject if any lane != 0xFF).
		b.Raw("VORR V17.B16, V16.B16, V19.B16").Raw("VORR V18.B16, V19.B16, V19.B16") // V19 = valid
		b.Raw("VAND V19.B16, V7.B16, V7.B16")                                         // good &= valid
		// nibble = ((c-'0')&isDigit) | ((c-bU)&isUpper) | ((c-bL)&isLower).
		b.Raw("VSUB V26.B16, %s, V2.B16", vc).Raw("VAND V16.B16, V2.B16, V5.B16")
		b.Raw("VSUB V27.B16, %s, V3.B16", vc).Raw("VAND V17.B16, V3.B16, V6.B16").Raw("VORR V6.B16, V5.B16, V5.B16")
		b.Raw("VSUB V28.B16, %s, V3.B16", vc).Raw("VAND V18.B16, V3.B16, V6.B16").Raw("VORR V6.B16, V5.B16, V5.B16")
		b.Raw("VMOV V5.B16, %s", vc) // plane register now holds nibbles
	}

	classify("V0.B16") // hi-nibble plane -> V0 = hi nibbles
	classify("V1.B16") // lo-nibble plane -> V1 = lo nibbles

	// Reject the block if any char was invalid: V7 is all-ones iff every lane was
	// valid. Reduce both halves with AND, invert, and bail if the result is
	// nonzero (i.e. some lane was not 0xFF).
	b.Raw("VMOV V7.D[0], R5").
		Raw("VMOV V7.D[1], R6").
		Raw("AND R6, R5, R5").
		Raw("MVN R5, R5").
		Raw("CBNZ R5, done").
		// Fuse: out = (hi<<4) | lo.
		Raw("VSHL $4, V0.B16, V0.B16").
		Raw("VORR V1.B16, V0.B16, V0.B16").
		Raw("VST1.P [V0.B16], 16(R0)"). // store 16 bytes, advance dst
		Raw("ADD $32, R1").             // advance src by 32 chars
		Raw("ADD $1, R3").
		Raw("SUB $1, R2").
		Raw("CBNZ R2, loop").
		Label("done").
		Raw("MOVD R3, ret+56(FP)").
		Ret()
	f.Add(b.Func())

	if err := os.WriteFile("decode_arm64.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote decode_arm64.s")
}
