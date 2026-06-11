//go:build ignore

// Command gen produces decode_s390x.s with go-asmgen: a vectorised hex (base16)
// decoder for s390x (IBM Z, vector facility, BIG-ENDIAN) accepting the same
// alphabet as encoding/hex ('0'-'9','a'-'f','A'-'F'). Two input chars -> one
// output byte; 32 input chars -> 16 output bytes per loop.
//
// Method (mirrors the amd64 decoder, with VCHLB unsigned range tests and a VPERM
// gather instead of PMADDUBSW):
//   - Load two 16-char halves with VL. On big-endian s390x lane 0 == the first
//     memory byte (the high-order/leftmost lane), so lanes are in natural memory
//     order and VST mirrors it.
//   - Per half, a char is valid iff it is in ['0','9'] OR ['A','F'] OR ['a','f'].
//     Each range test is two unsigned compares VCHLB(c,lo-1) & VCHLB(hi+1,c)
//     AND'd (VCHLB Va,Vb,Vt sets Vt = Va>Vb unsigned); the three masks OR into a
//     per-char valid mask.
//   - Nibble value: digit c-'0'; upper c-('A'-10); lower c-('a'-10); each
//     subtraction (VSB) is masked by its range and OR'd, so valid chars get their
//     0..15 nibble and invalid chars get junk (discarded — see fallback).
//   - Invalid detection: accumulate ~valid into a "bad" vector across both
//     halves; if any byte is nonzero (tested by extracting both doublewords with
//     VLGVG) the block holds an invalid char, the kernel STOPS and returns the
//     count of fully-decoded blocks. The Go wrapper then resumes a scalar
//     encoding/hex-style loop so the InvalidByteError offset is bit-exact.
//   - Fuse: gather the even-indexed (high) nibbles and odd-indexed (low) nibbles
//     with two VPERMs, shift hi left 4 (VESLB) and OR with lo to form 16 result
//     bytes, then VST.
//
// BIG-ENDIAN NOTE: because VL lane 0 == first memory byte == leftmost lane, the
// even-index/odd-index gather control vectors (and VLGVG doubleword extraction)
// use the same natural-order indices as a natural-order little-endian load; the
// FuzzDecode test (error-identical to encoding/hex) is the gate.
//
// Signature: decodeBlocksVX(dst, src []byte, n int) int — n = number of 32-char
// blocks to attempt; returns the number of blocks fully decoded before the first
// block containing an invalid char (== n when all blocks are valid).
//
// Run: GOWORK=off go run decode_s390x_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/emit"
	"github.com/go-asmgen/asmgen/s390x"
)

func repByte(x byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = x
	}
	return b
}

func sigRet() abi.Signature {
	return abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		[]abi.Arg{abi.Scalar("ret", abi.Int64)},
	)
}

func main() {
	f := emit.NewFile("s390x")

	c0lo := f.Data("z_c0lo", repByte('0'-1, 16))
	c9hi := f.Data("z_c9hi", repByte('9'+1, 16))
	cUlo := f.Data("z_cUlo", repByte('A'-1, 16))
	cUhi := f.Data("z_cUhi", repByte('F'+1, 16))
	cLlo := f.Data("z_cLlo", repByte('a'-1, 16))
	cLhi := f.Data("z_cLhi", repByte('f'+1, 16))
	subD := f.Data("z_subD", repByte('0', 16))
	subU := f.Data("z_subU", repByte('A'-10, 16))
	subL := f.Data("z_subL", repByte('a'-10, 16))
	gHi := f.Data("z_gHi", []byte{0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30})
	gLo := f.Data("z_gLo", []byte{1, 3, 5, 7, 9, 11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31})

	b := s390x.NewFunc("decodeBlocksVX", sigRet(), 0)
	b.LoadArg("dst_base", "R1").LoadArg("src_base", "R2").LoadArg("n", "R3").
		Raw("MOVD $0, R4"). // R4 = blocks done (return value)
		Raw("CMPBEQ R3, $0, dret").
		// Load constants into vector registers once. (V20 is the bad accumulator,
		// reset each iteration; V0/V1 hold the two char halves.)
		Raw("MOVD $%s+0(SB), R5", c0lo).Raw("VL (R5), V8").  // '0'-1
		Raw("MOVD $%s+0(SB), R5", c9hi).Raw("VL (R5), V9").  // '9'+1
		Raw("MOVD $%s+0(SB), R5", cUlo).Raw("VL (R5), V10"). // 'A'-1
		Raw("MOVD $%s+0(SB), R5", cUhi).Raw("VL (R5), V11"). // 'F'+1
		Raw("MOVD $%s+0(SB), R5", cLlo).Raw("VL (R5), V12"). // 'a'-1
		Raw("MOVD $%s+0(SB), R5", cLhi).Raw("VL (R5), V13"). // 'f'+1
		Raw("MOVD $%s+0(SB), R5", subD).Raw("VL (R5), V14"). // '0'
		Raw("MOVD $%s+0(SB), R5", subU).Raw("VL (R5), V15"). // 'A'-10
		Raw("MOVD $%s+0(SB), R5", subL).Raw("VL (R5), V16"). // 'a'-10
		Raw("MOVD $%s+0(SB), R5", gHi).Raw("VL (R5), V18").  // gatherHi
		Raw("MOVD $%s+0(SB), R5", gLo).Raw("VL (R5), V19").  // gatherLo
		Label("dloop").
		Raw("VL (R2), V0").     // chars 0..15
		Raw("VL 16(R2), V1").   // chars 16..31
		Raw("VZERO V20")        // bad accumulator = 0

	// half decodes 16 chars in V(xc) -> nibble values, OR-ing ~valid into V20.
	// VCHLB Va, Vb, Vt sets Vt = (Va > Vb) unsigned, per byte.
	half := func(xc string) {
		// isDigit = (c > '0'-1) & ('9'+1 > c)
		b.Raw("VCHLB %s, V8, V21", xc)        // V21 = c > '0'-1
		b.Raw("VCHLB V9, %s, V22", xc)        // V22 = '9'+1 > c
		b.Raw("VN V21, V22, V21")             // V21 = isDigit
		// isUpper
		b.Raw("VCHLB %s, V10, V22", xc)
		b.Raw("VCHLB V11, %s, V23", xc)
		b.Raw("VN V22, V23, V22")             // V22 = isUpper
		// isLower
		b.Raw("VCHLB %s, V12, V23", xc)
		b.Raw("VCHLB V13, %s, V24", xc)
		b.Raw("VN V23, V24, V23")             // V23 = isLower
		// valid = isDigit | isUpper | isLower ; bad |= ~valid
		b.Raw("VO V21, V22, V24")
		b.Raw("VO V24, V23, V24")             // V24 = valid
		b.Raw("VNO V24, V24, V25")            // V25 = ~valid (NOR with itself = NOT)
		b.Raw("VO V20, V25, V20")             // accumulate bad
		// nibble = (c-'0')&isDigit | (c-('A'-10))&isUpper | (c-('a'-10))&isLower
		// VSB From, Reg, To computes To = Reg - From, so put the constant first.
		b.Raw("VSB V14, %s, V26", xc).Raw("VN V26, V21, V26") // digit: c-'0'
		b.Raw("VSB V15, %s, V27", xc).Raw("VN V27, V22, V27").Raw("VO V26, V27, V26")
		b.Raw("VSB V16, %s, V27", xc).Raw("VN V27, V23, V27").Raw("VO V26, V27, V26")
		b.Raw("VLR V26, %s", xc) // nibble values back into xc
	}
	half("V0")
	half("V1")

	// Any invalid char? Extract both doublewords of V20 and test.
	b.Raw("VLGVG $0, V20, R5")
	b.Raw("VLGVG $1, V20, R6")
	b.Raw("OR R6, R5, R5")
	b.Raw("CMPBNE R5, $0, dret")

	// Fuse: gather hi nibbles and lo nibbles, hi<<4 | lo.
	b.Raw("VPERM V0, V1, V18, V21") // V21 = high nibbles of all 16 bytes
	b.Raw("VPERM V0, V1, V19, V22") // V22 = low nibbles
	b.Raw("VESLB $4, V21, V21")     // V21 = hi << 4
	b.Raw("VO V21, V22, V21")       // V21 = result bytes
	b.Raw("VST V21, (R1)")          // store 16 result bytes
	b.Raw("ADD $32, R2")
	b.Raw("ADD $16, R1")
	b.Raw("ADD $1, R4")
	b.Raw("CMPBNE R4, R3, dloop")
	b.Label("dret").StoreRet("R4", "ret").Ret()
	f.Add(b.Func())

	if err := os.WriteFile("decode_s390x.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote decode_s390x.s")
}
