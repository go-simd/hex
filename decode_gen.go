//go:build ignore

// Command gen produces decode_amd64.s with go-asmgen: a vectorised hex (base16)
// decoder accepting the same alphabet as encoding/hex ('0'-'9','a'-'f','A'-'F').
// Two input hex chars become one output byte. Per 32 input chars the SSE path
// emits 16 bytes; the AVX2 path emits 32 bytes per 64-char iteration.
//
// Method (per 16-char half):
//   - Validity: a char is valid iff it lies in ['0','9'] OR ['A','F'] OR
//     ['a','f']. Each range test is two PCMPGTB (signed; all hex chars < 0x80
//     so the sign bit is irrelevant) AND'd together; the three range masks are
//     OR'd into a per-char "valid" mask (0xFF valid / 0x00 invalid).
//   - Nibble value: digit -> c-'0'; upper -> c-('A'-10); lower -> c-('a'-10).
//     Each subtraction is masked by its range mask and the three are OR'd, so
//     every valid char yields its 0..15 nibble and invalid chars yield junk
//     (discarded — see fallback).
//   - The per-half "valid" masks are inverted and accumulated into a "bad"
//     register. If, after both halves, PMOVMSKB(bad) != 0 the block contains an
//     invalid char: the kernel STOPS and returns the count of fully-decoded
//     blocks. The Go wrapper then resumes a scalar encoding/hex-style loop from
//     that block, so the reported InvalidByteError offset is bit-exact and no
//     SIMD pin-pointing is needed.
//   - Fuse: PMADDUBSW with a {16,1,16,1,...} multiplier turns adjacent
//     (hi,lo) nibble bytes into a 16-bit hi*16+lo == (hi<<4)|lo (each nibble
//     <16 so no overflow). PACKUSWB the two halves' 8 words each into 16 bytes.
//
// Signature: decodeBlocksSSE(dst, src []byte, n int) int — n = number of
// 32-char blocks to attempt; returns the number of blocks fully decoded before
// the first block containing an invalid char (== n when all blocks are valid).
//
// Run: go run decode_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/amd64"
	"github.com/go-asmgen/asmgen/emit"
)

func repByte(x byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = x
	}
	return b
}

func rep(v []byte, times int) []byte {
	var b []byte
	for i := 0; i < times; i++ {
		b = append(b, v...)
	}
	return b
}

// maddPat is the PMADDUBSW multiplier fusing each (hi,lo) nibble pair into one
// byte: even (high) lanes *16, odd (low) lanes *1.
func maddPat() []byte {
	b := make([]byte, 16)
	for i := 0; i < 16; i += 2 {
		b[i] = 16
		b[i+1] = 1
	}
	return b
}

func sig() abi.Signature {
	return abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		[]abi.Arg{abi.Scalar("ret", abi.Int64)},
	)
}

func main() {
	f := emit.NewFile("amd64")

	// 16-byte SSE constants.
	c0lo := f.Data("c0lo", repByte('0'-1, 16)) // PCMPGTB c, c0lo  => c >= '0'
	c9hi := f.Data("c9hi", repByte('9'+1, 16)) // PCMPGTB c9hi, c  => c <= '9'
	cUlo := f.Data("cUlo", repByte('A'-1, 16))
	cUhi := f.Data("cUhi", repByte('F'+1, 16))
	cLlo := f.Data("cLlo", repByte('a'-1, 16))
	cLhi := f.Data("cLhi", repByte('f'+1, 16))
	sub0 := f.Data("subD", repByte('0', 16))
	subU := f.Data("subU", repByte('A'-10, 16))
	subL := f.Data("subL", repByte('a'-10, 16))
	madd := f.Data("madd", maddPat())

	genSSE(f, c0lo, c9hi, cUlo, cUhi, cLlo, cLhi, sub0, subU, subL, madd)

	// 32-byte AVX2 constants (each pattern broadcast to both 128-bit lanes).
	v0lo := f.Data("v0lo", repByte('0'-1, 32))
	v9hi := f.Data("v9hi", repByte('9'+1, 32))
	vUlo := f.Data("vUlo", repByte('A'-1, 32))
	vUhi := f.Data("vUhi", repByte('F'+1, 32))
	vLlo := f.Data("vLlo", repByte('a'-1, 32))
	vLhi := f.Data("vLhi", repByte('f'+1, 32))
	vsub0 := f.Data("vsubD", repByte('0', 32))
	vsubU := f.Data("vsubU", repByte('A'-10, 32))
	vsubL := f.Data("vsubL", repByte('a'-10, 32))
	vmadd := f.Data("vmadd", rep(maddPat(), 2))

	genAVX2(f, v0lo, v9hi, vUlo, vUhi, vLlo, vLhi, vsub0, vsubU, vsubL, vmadd)

	if err := os.WriteFile("decode_amd64.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote decode_amd64.s")
}

// genSSE emits decodeBlocksSSE: 32 chars -> 16 bytes/block.
func genSSE(f *emit.File, c0lo, c9hi, cUlo, cUhi, cLlo, cLhi, sub0, subU, subL, madd string) {
	b := amd64.NewFunc("decodeBlocksSSE", sig(), 0)
	b.LoadArg("dst_base", "DI").LoadArg("src_base", "SI").LoadArg("n", "CX").
		Raw("XORQ BX, BX").
		Raw("TESTQ CX, CX").Raw("JZ sret").
		Label("sloop").
		Raw("MOVOU (SI), X0").  // chars 0..15  (low half)
		Raw("MOVOU 16(SI), X1") // chars 16..31 (high half)

	// All-ones for inverting the valid mask; bad accumulator reset to 0.
	b.Raw("PCMPEQB X15, X15"). // X15 = all ones
					Raw("PXOR X9, X9") // X9 = bad = 0

	// halfImpl decodes 16 chars in xc -> nibble values in xc, OR-ing per-char
	// invalid bits into X9. Scratch: X8 (isLower), X10..X14. X15/X9 preserved.
	halfImpl := func(xc string) {
		b.Raw("MOVO %s, X10", xc).Raw("MOVOU %s+0(SB), X11", c0lo).Raw("PCMPGTB X11, X10")
		b.Raw("MOVOU %s+0(SB), X12", c9hi).Raw("PCMPGTB %s, X12", xc).Raw("PAND X12, X10") // X10=isDigit
		b.Raw("MOVO %s, X12", xc).Raw("MOVOU %s+0(SB), X11", cUlo).Raw("PCMPGTB X11, X12")
		b.Raw("MOVOU %s+0(SB), X13", cUhi).Raw("PCMPGTB %s, X13", xc).Raw("PAND X13, X12") // X12=isUpper
		b.Raw("MOVO %s, X8", xc).Raw("MOVOU %s+0(SB), X11", cLlo).Raw("PCMPGTB X11, X8")
		b.Raw("MOVOU %s+0(SB), X13", cLhi).Raw("PCMPGTB %s, X13", xc).Raw("PAND X13, X8") // X8=isLower
		// valid = isDigit|isUpper|isLower
		b.Raw("MOVO X10, X14").Raw("POR X12, X14").Raw("POR X8, X14")
		// bad |= ~valid
		b.Raw("MOVO X14, X11").Raw("PXOR X15, X11").Raw("POR X11, X9")
		// nibble
		b.Raw("MOVO %s, X11", xc).Raw("MOVOU %s+0(SB), X14", sub0).Raw("PSUBB X14, X11").Raw("PAND X10, X11")
		b.Raw("MOVO %s, X14", xc).Raw("MOVOU %s+0(SB), X13", subU).Raw("PSUBB X13, X14").Raw("PAND X12, X14").Raw("POR X14, X11")
		b.Raw("MOVO %s, X14", xc).Raw("MOVOU %s+0(SB), X13", subL).Raw("PSUBB X13, X14").Raw("PAND X8, X14").Raw("POR X14, X11")
		b.Raw("MOVO X11, %s", xc)
	}

	halfImpl("X0")
	halfImpl("X1")

	// Any invalid char? PMOVMSKB(bad) != 0 -> stop, return BX (blocks done).
	b.Raw("PMOVMSKB X9, AX").Raw("TESTL AX, AX").Raw("JNZ sret")

	// Fuse nibbles -> bytes. X0,X1 hold nibble values (0..15) at every byte.
	b.Raw("MOVOU %s+0(SB), X14", madd) // {16,1,...}
	b.Raw("PMADDUBSW X14, X0")         // 8 words: hi*16+lo for chars 0..15
	b.Raw("MOVOU %s+0(SB), X14", madd)
	b.Raw("PMADDUBSW X14, X1") // 8 words for chars 16..31
	b.Raw("PACKUSWB X1, X0")   // 16 bytes (each word < 256, no saturation)
	b.Raw("MOVOU X0, (DI)")

	b.Raw("ADDQ $32, SI").Raw("ADDQ $16, DI").Raw("INCQ BX").Raw("CMPQ BX, CX").Raw("JNE sloop")
	b.Label("sret").StoreRet("BX", "ret").Ret()
	f.Add(b.Func())
}

// genAVX2 emits decodeBlocksAVX2: 64 chars -> 32 bytes/block (per-lane logic
// mirrors SSE; PACKUSWB and PMADDUBSW are per-128-bit-lane so we fix the lane
// interleave with VPERMQ before storing).
func genAVX2(f *emit.File, v0lo, v9hi, vUlo, vUhi, vLlo, vLhi, sub0, subU, subL, madd string) {
	b := amd64.NewFunc("decodeBlocksAVX2", sig(), 0)
	b.LoadArg("dst_base", "DI").LoadArg("src_base", "SI").LoadArg("n", "CX").
		Raw("XORQ BX, BX").
		Raw("TESTQ CX, CX").Raw("JZ vret").
		Raw("VPCMPEQB Y15, Y15, Y15"). // all-ones
		Label("vloop").
		Raw("VMOVDQU (SI), Y0").   // chars 0..31
		Raw("VMOVDQU 32(SI), Y1"). // chars 32..63
		Raw("VPXOR Y9, Y9, Y9")    // bad = 0

	half := func(yc string) {
		// isDigit
		b.Raw("VMOVDQU %s+0(SB), Y11", v0lo).Raw("VPCMPGTB Y11, %s, Y10", yc)
		b.Raw("VMOVDQU %s+0(SB), Y12", v9hi).Raw("VPCMPGTB %s, Y12, Y12", yc).Raw("VPAND Y12, Y10, Y10") // Y10=isDigit
		// isUpper
		b.Raw("VMOVDQU %s+0(SB), Y11", vUlo).Raw("VPCMPGTB Y11, %s, Y12", yc)
		b.Raw("VMOVDQU %s+0(SB), Y13", vUhi).Raw("VPCMPGTB %s, Y13, Y13", yc).Raw("VPAND Y13, Y12, Y12") // Y12=isUpper
		// isLower (kept in Y8)
		b.Raw("VMOVDQU %s+0(SB), Y11", vLlo).Raw("VPCMPGTB Y11, %s, Y8", yc)
		b.Raw("VMOVDQU %s+0(SB), Y13", vLhi).Raw("VPCMPGTB %s, Y13, Y13", yc).Raw("VPAND Y13, Y8, Y8") // Y8=isLower
		// valid -> bad |= ~valid
		b.Raw("VPOR Y12, Y10, Y14").Raw("VPOR Y8, Y14, Y14")
		b.Raw("VPXOR Y15, Y14, Y11").Raw("VPOR Y11, Y9, Y9")
		// nibble
		b.Raw("VMOVDQU %s+0(SB), Y14", sub0).Raw("VPSUBB Y14, %s, Y11", yc).Raw("VPAND Y10, Y11, Y11")
		b.Raw("VMOVDQU %s+0(SB), Y14", subU).Raw("VPSUBB Y14, %s, Y13", yc).Raw("VPAND Y12, Y13, Y13").Raw("VPOR Y13, Y11, Y11")
		b.Raw("VMOVDQU %s+0(SB), Y14", subL).Raw("VPSUBB Y14, %s, Y13", yc).Raw("VPAND Y8, Y13, Y13").Raw("VPOR Y13, Y11, Y11")
		b.Raw("VMOVDQA Y11, %s", yc)
	}
	half("Y0")
	half("Y1")

	b.Raw("VPMOVMSKB Y9, AX").Raw("TESTL AX, AX").Raw("JNZ vret")

	// fuse
	b.Raw("VMOVDQU %s+0(SB), Y14", madd)
	b.Raw("VPMADDUBSW Y0, Y14, Y0"). // per-lane 8 words each (lanes independent)
						Raw("VPMADDUBSW Y1, Y14, Y1").
						Raw("VPACKUSWB Y1, Y0, Y0") // 32 bytes but lane-interleaved
	// VPACKUSWB on YMM packs per-128-bit lane: result lane0 = pack(Y0.lo,Y1.lo),
	// lane1 = pack(Y0.hi,Y1.hi). Desired byte order is chars 0..31 then 32..63,
	// i.e. [Y0.lo, Y0.hi, Y1.lo, Y1.hi] -> qwords 0,2,1,3. Fix with VPERMQ.
	b.Raw("VPERMQ $0xD8, Y0, Y0") // 11 01 10 00 -> qwords 0,2,1,3
	b.Raw("VMOVDQU Y0, (DI)")

	b.Raw("ADDQ $64, SI").Raw("ADDQ $32, DI").Raw("INCQ BX").Raw("CMPQ BX, CX").Raw("JNE vloop")
	b.Label("vret").Raw("VZEROUPPER").StoreRet("BX", "ret").Ret()
	f.Add(b.Func())
}
