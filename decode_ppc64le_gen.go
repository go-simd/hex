//go:build ignore

// Command gen produces decode_ppc64le.s with go-asmgen: a vectorised hex
// (base16) decoder for ppc64le (POWER, VSX/AltiVec) accepting the same alphabet
// as encoding/hex ('0'-'9','a'-'f','A'-'F'). Two input chars -> one output byte;
// 32 input chars -> 16 output bytes per loop iteration.
//
// Method (mirrors the amd64 decoder, with VCMPGTUB range tests and a VPERM
// gather instead of PMADDUBSW):
//   - Load two 16-char halves with LXVB16X (natural byte order on ppc64le).
//   - Per half, a char is valid iff it is in ['0','9'] OR ['A','F'] OR ['a','f'].
//     Each range test is two unsigned compares VCMPGTUB(c,lo-1) & VCMPGTUB(hi+1,c)
//     AND'd; the three masks are OR'd into a per-char valid mask.
//   - Nibble value: digit c-'0'; upper c-('A'-10); lower c-('a'-10); each
//     subtraction (VSUBUBM) is masked by its range and OR'd, so valid chars get
//     their 0..15 nibble and invalid chars get junk (discarded — see fallback).
//   - Invalid detection: accumulate ~valid into a "bad" vector across both
//     halves; if any byte is nonzero the block holds an invalid char, the kernel
//     STOPS and returns the count of fully-decoded blocks. The Go wrapper then
//     resumes a scalar encoding/hex-style loop so the InvalidByteError offset is
//     bit-exact.
//   - Fuse: gather the even-indexed (high) nibbles and odd-indexed (low) nibbles
//     of the 32 chars with two VPERMs into one 16-byte hi vector and one lo
//     vector, shift hi left 4 (VSLB) and OR with lo to form 16 result bytes,
//     then STXVB16X.
//
// Signature: decodeBlocksVSX(dst, src []byte, n int) int — n = number of 32-char
// blocks to attempt; returns the number of blocks fully decoded before the first
// block containing an invalid char (== n when all blocks are valid).
//
// Run: GOWORK=off go run decode_ppc64le_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/emit"
	"github.com/go-asmgen/asmgen/ppc64"
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
	f := emit.NewFile("ppc64le")

	c0lo := f.Data("d_c0lo", repByte('0'-1, 16))
	c9hi := f.Data("d_c9hi", repByte('9'+1, 16))
	cUlo := f.Data("d_cUlo", repByte('A'-1, 16))
	cUhi := f.Data("d_cUhi", repByte('F'+1, 16))
	cLlo := f.Data("d_cLlo", repByte('a'-1, 16))
	cLhi := f.Data("d_cLhi", repByte('f'+1, 16))
	subD := f.Data("d_subD", repByte('0', 16))
	subU := f.Data("d_subU", repByte('A'-10, 16))
	subL := f.Data("d_subL", repByte('a'-10, 16))
	four := f.Data("d_four", repByte(4, 16))
	// gatherHi picks the high (even-index) nibble bytes of the two 16-char
	// halves: bytes 0,2,4,...,14 of half0 then 0,2,...,14 of half1.
	// VPERM(v0half, v1half, ctrl): index 0..15 -> half0 byte i; 16..31 -> half1.
	gHi := f.Data("d_gHi", []byte{0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30})
	gLo := f.Data("d_gLo", []byte{1, 3, 5, 7, 9, 11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31})

	b := ppc64.NewFunc("decodeBlocksVSX", sigRet(), 0)
	b.LoadArg("dst_base", "R3").LoadArg("src_base", "R4").LoadArg("n", "R5").
		// Constant base addresses in R6.. (re-used per iteration via LXVB16X).
		Raw("MOVD $0, R7"). // R7 = blocks done (return value)
		Raw("CMP R5, $0").Raw("BEQ dret").
		Raw("MOVD $0, R8").  // src byte offset
		Raw("MOVD $0, R9").  // dst byte offset
		// Load constants into V registers once.
		Raw("MOVD $%s+0(SB), R10", c0lo).Raw("LXVB16X (R0)(R10), VS40").  // V8  = '0'-1
		Raw("MOVD $%s+0(SB), R10", c9hi).Raw("LXVB16X (R0)(R10), VS41").  // V9  = '9'+1
		Raw("MOVD $%s+0(SB), R10", cUlo).Raw("LXVB16X (R0)(R10), VS42").  // V10 = 'A'-1
		Raw("MOVD $%s+0(SB), R10", cUhi).Raw("LXVB16X (R0)(R10), VS43").  // V11 = 'F'+1
		Raw("MOVD $%s+0(SB), R10", cLlo).Raw("LXVB16X (R0)(R10), VS44").  // V12 = 'a'-1
		Raw("MOVD $%s+0(SB), R10", cLhi).Raw("LXVB16X (R0)(R10), VS45").  // V13 = 'f'+1
		Raw("MOVD $%s+0(SB), R10", subD).Raw("LXVB16X (R0)(R10), VS46").  // V14 = '0'
		Raw("MOVD $%s+0(SB), R10", subU).Raw("LXVB16X (R0)(R10), VS47").  // V15 = 'A'-10
		Raw("MOVD $%s+0(SB), R10", subL).Raw("LXVB16X (R0)(R10), VS48").  // V16 = 'a'-10
		Raw("MOVD $%s+0(SB), R10", four).Raw("LXVB16X (R0)(R10), VS49").  // V17 = 4
		Raw("MOVD $%s+0(SB), R10", gHi).Raw("LXVB16X (R0)(R10), VS50").   // V18 = gatherHi
		Raw("MOVD $%s+0(SB), R10", gLo).Raw("LXVB16X (R0)(R10), VS51").   // V19 = gatherLo
		Label("dloop").
		Raw("LXVB16X (R8)(R4), VS32").    // V0 = chars 0..15
		Raw("ADD $16, R8").
		Raw("LXVB16X (R8)(R4), VS33").    // V1 = chars 16..31
		Raw("ADD $-16, R8").
		Raw("VXOR V20, V20, V20")         // V20 = bad accumulator = 0

	// half decodes 16 chars in V(xc) -> nibble values, OR-ing ~valid into V20.
	// Scratch: V21..V27. xc is the V register number string.
	half := func(xc string) {
		// isDigit = (c > '0'-1) & ('9'+1 > c)
		b.Raw("VCMPGTUB %s, V8, V21", xc)        // V21 = c > '0'-1
		b.Raw("VCMPGTUB V9, %s, V22", xc)        // V22 = '9'+1 > c
		b.Raw("VAND V21, V22, V21")              // V21 = isDigit
		// isUpper
		b.Raw("VCMPGTUB %s, V10, V22", xc)
		b.Raw("VCMPGTUB V11, %s, V23", xc)
		b.Raw("VAND V22, V23, V22")              // V22 = isUpper
		// isLower
		b.Raw("VCMPGTUB %s, V12, V23", xc)
		b.Raw("VCMPGTUB V13, %s, V24", xc)
		b.Raw("VAND V23, V24, V23")              // V23 = isLower
		// valid = isDigit | isUpper | isLower ; bad |= ~valid
		b.Raw("VOR V21, V22, V24")
		b.Raw("VOR V24, V23, V24")               // V24 = valid mask
		b.Raw("VNOR V24, V24, V25")              // V25 = ~valid
		b.Raw("VOR V20, V25, V20")               // accumulate bad
		// nibble = (c-'0')&isDigit | (c-('A'-10))&isUpper | (c-('a'-10))&isLower
		b.Raw("VSUBUBM %s, V14, V26", xc).Raw("VAND V26, V21, V26") // digit part
		b.Raw("VSUBUBM %s, V15, V27", xc).Raw("VAND V27, V22, V27").Raw("VOR V26, V27, V26")
		b.Raw("VSUBUBM %s, V16, V27", xc).Raw("VAND V27, V23, V27").Raw("VOR V26, V27, V26")
		b.Raw("VOR V26, V26, %s", xc) // write nibble values back into xc
	}
	half("V0")
	half("V1")

	// Any invalid char? Reduce V20 (=VS52) to a GPR and test. Use V28 (=VS60) as
	// the doubleword-swap scratch so V21/V22 stay free for the fuse below.
	b.Raw("MFVSRD VS52, R10")              // R10 = doubleword 0 of V20
	b.Raw("XXPERMDI VS52, VS52, $2, VS60") // V28 = V20 with doublewords swapped
	b.Raw("MFVSRD VS60, R11")              // R11 = the other doubleword
	b.Raw("OR R11, R10, R10")
	b.Raw("CMP R10, $0").Raw("BNE dret")

	// Fuse: gather hi nibbles and lo nibbles, hi<<4 | lo.
	b.Raw("VPERM V0, V1, V18, V21")  // V21 = high nibbles of all 16 bytes
	b.Raw("VPERM V0, V1, V19, V22")  // V22 = low nibbles
	b.Raw("VSLB V21, V17, V21")      // V21 = hi << 4
	b.Raw("VOR V21, V22, V21")       // V21 = result bytes
	b.Raw("STXVB16X VS53, (R9)(R3)") // store V21 (=VS53)
	b.Raw("ADD $32, R8")
	b.Raw("ADD $16, R9")
	b.Raw("ADD $1, R7")
	b.Raw("CMP R7, R5").Raw("BNE dloop")
	b.Label("dret").StoreRet("R7", "ret").Ret()
	f.Add(b.Func())

	if err := os.WriteFile("decode_ppc64le.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote decode_ppc64le.s")
}
