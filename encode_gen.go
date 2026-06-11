//go:build ignore

// Command gen produces encode_amd64.s with go-asmgen: a vectorised lowercase
// hex (base16) encoder. Each input byte becomes two ASCII chars. Per 16 input
// bytes the SSE path emits 32 chars; the AVX2 path doubles that to 32 input ->
// 64 chars per iteration.
//
// Method: split every byte into its high and low nibble (PSRLW+PAND for the
// high nibble, PAND for the low one), map each nibble (0..15) to its ASCII via
// a PSHUFB lookup of the table "0123456789abcdef", then interleave the two
// nibble streams with PUNPCKLBW/PUNPCKHBW so each input byte's (hi,lo) chars
// land adjacent, and store. Constants are emitted via emit.File.Data.
//
// Run: go run encode_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/amd64"
	"github.com/go-asmgen/asmgen/emit"
)

func rep(v []byte, times int) []byte {
	var b []byte
	for i := 0; i < times; i++ {
		b = append(b, v...)
	}
	return b
}

func repByte(x byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = x
	}
	return b
}

// hexLUT maps nibble 0..15 -> lowercase ASCII hex digit.
var hexLUT = []byte("0123456789abcdef")

func sig() abi.Signature {
	return abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		nil,
	)
}

func main() {
	f := emit.NewFile("amd64")

	// ---- SSE2/SSSE3: 16 input bytes -> 32 hex chars per block ----
	lut := f.Data("hexlut", hexLUT)             // 16-byte PSHUFB nibble table
	lo4 := f.Data("lomask", repByte(0x0f, 16))  // low-nibble mask

	s := amd64.NewFunc("encodeBlocksSSE", sig(), 0)
	s.LoadArg("dst_base", "DI").LoadArg("src_base", "SI").LoadArg("n", "CX").
		Raw("MOVOU %s+0(SB), X7", lut).
		Raw("MOVOU %s+0(SB), X8", lo4).
		Raw("TESTQ CX, CX").Raw("JZ sdone").
		Label("sloop").
		Raw("MOVOU (SI), X0").       // 16 source bytes
		Raw("MOVO X0, X1").          // X1 = bytes (for low nibble)
		Raw("PSRLW $4, X0").         // X0 = bytes>>4 (word shift; high bits are junk)
		Raw("PAND X8, X0").          // X0 = high nibble (0..15)
		Raw("PAND X8, X1").          // X1 = low nibble (0..15)
		Raw("MOVO X7, X2").Raw("PSHUFB X0, X2"). // X2 = ASCII of high nibbles
		Raw("MOVO X7, X3").Raw("PSHUFB X1, X3"). // X3 = ASCII of low nibbles
		// Interleave hi/lo so output is hi0,lo0,hi1,lo1,... PUNPCKLBW dst,src
		// computes dst[2i]=dst[i], dst[2i+1]=src[i]; here dst=hi, src=lo.
		Raw("MOVO X2, X4").Raw("PUNPCKLBW X3, X4"). // chars for bytes 0..7
		Raw("PUNPCKHBW X3, X2").                    // chars for bytes 8..15
		Raw("MOVOU X4, (DI)").
		Raw("MOVOU X2, 16(DI)").
		Raw("ADDQ $16, SI").Raw("ADDQ $32, DI").Raw("DECQ CX").Raw("JNZ sloop").
		Label("sdone").Ret()
	f.Add(s.Func())

	// ---- AVX2: 32 input bytes -> 64 hex chars per block ----
	// VPUNPCK*BW operate per 128-bit lane, so the interleave of a 256-bit
	// register keeps lane0 (src bytes 0..15) and lane1 (src bytes 16..31)
	// independent. Within each lane the byte order is already correct; the two
	// stores below land lane-low then lane-high, which is exactly the natural
	// order because VMOVDQU writes the whole 256-bit interleave contiguously.
	lutb := f.Data("hexlutb", rep(hexLUT, 2)) // table broadcast to both lanes
	lo4b := f.Data("lomaskb", repByte(0x0f, 32))

	vv := amd64.NewFunc("encodeBlocksAVX2", sig(), 0)
	vv.LoadArg("dst_base", "DI").LoadArg("src_base", "SI").LoadArg("n", "CX").
		Raw("VMOVDQU %s+0(SB), Y7", lutb).
		Raw("VMOVDQU %s+0(SB), Y8", lo4b).
		Raw("TESTQ CX, CX").Raw("JZ vdone").
		Label("vloop").
		Raw("VMOVDQU (SI), Y0").     // 32 source bytes
		Raw("VPSRLW $4, Y0, Y1").    // Y1 = bytes>>4
		Raw("VPAND Y8, Y1, Y1").     // Y1 = high nibble
		Raw("VPAND Y8, Y0, Y0").     // Y0 = low nibble
		Raw("VPSHUFB Y1, Y7, Y1").   // Y1 = ASCII high nibbles
		Raw("VPSHUFB Y0, Y7, Y0").   // Y0 = ASCII low nibbles
		Raw("VPUNPCKLBW Y0, Y1, Y2"). // per-lane: chars for bytes {0..7, 16..23}
		Raw("VPUNPCKHBW Y0, Y1, Y3"). // per-lane: chars for bytes {8..15, 24..31}
		// Reassemble in memory order. Output bytes 0..31 = lane0 of Y2 then
		// lane0 of Y3; bytes 32..63 = lane1 of Y2 then lane1 of Y3.
		Raw("VPERM2I128 $0x20, Y3, Y2, Y4"). // [Y2.lo, Y3.lo] -> chars 0..31
		Raw("VPERM2I128 $0x31, Y3, Y2, Y5"). // [Y2.hi, Y3.hi] -> chars 32..63
		Raw("VMOVDQU Y4, (DI)").
		Raw("VMOVDQU Y5, 32(DI)").
		Raw("ADDQ $32, SI").Raw("ADDQ $64, DI").Raw("DECQ CX").Raw("JNZ vloop").
		Label("vdone").Raw("VZEROUPPER").Ret()
	f.Add(vv.Func())

	if err := os.WriteFile("encode_amd64.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote encode_amd64.s")
}
