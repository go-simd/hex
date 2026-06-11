//go:build ignore

// Command gen produces encode_ppc64le.s with go-asmgen: a vectorised lowercase
// hex (base16) encoder for ppc64le (POWER, VSX/AltiVec). Each input byte becomes
// two ASCII chars; 16 input bytes -> 32 hex chars per loop iteration.
//
// Method (mirrors the amd64 PSHUFB approach with VPERM as the table lookup):
//   - Load 16 source bytes with LXVD2X into a VSX register (aliased to a VMX V
//     register: Vn == VS(32+n)).
//   - Low nibble:  VAND src, lomask(0x0f).
//   - High nibble: VSRB src, four — a per-byte logical shift right by 4 yields
//     the high nibble (0..15) with no extra mask needed.
//   - Map each nibble to its ASCII digit with VPERM using the 16-entry table
//     "0123456789abcdef" as both permute sources; every index is 0..15 so the
//     lookup stays within the first source.
//   - Interleave so output is hi0,lo0,hi1,lo1,... using two VPERM passes driven
//     by control vectors that pick alternating bytes from the ASCII-hi and
//     ASCII-lo vectors; store the two halves with STXVD2X.
//
// The exact VPERM lane indices are endianness-sensitive on ppc64le; the
// position-dependent FuzzEncode test (byte-identical to encoding/hex) is the
// gate that the interleave control vectors are correct.
//
// Run: GOWORK=off go run encode_ppc64le_gen.go
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

var hexLUT = []byte("0123456789abcdef")

func sig() abi.Signature {
	return abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		nil,
	)
}

func main() {
	f := emit.NewFile("ppc64le")

	lut := f.Data("hexlut_p", hexLUT)
	lo4 := f.Data("lomask_p", repByte(0x0f, 16))
	four := f.Data("four_p", repByte(4, 16))
	ctrlLo := f.Data("ctrlLo_p", []byte{0, 16, 1, 17, 2, 18, 3, 19, 4, 20, 5, 21, 6, 22, 7, 23})
	ctrlHi := f.Data("ctrlHi_p", []byte{8, 24, 9, 25, 10, 26, 11, 27, 12, 28, 13, 29, 14, 30, 15, 31})

	g := ppc64.NewFunc("encodeBlocksVSX", sig(), 0)
	g.LoadArg("dst_base", "R3").LoadArg("src_base", "R4").LoadArg("n", "R5").
		Raw("MOVD $%s+0(SB), R6", lut).
		Raw("MOVD $%s+0(SB), R7", lo4).
		Raw("MOVD $%s+0(SB), R8", four).
		Raw("MOVD $%s+0(SB), R9", ctrlLo).
		Raw("MOVD $%s+0(SB), R10", ctrlHi).
		Raw("LXVB16X (R0)(R6), VS33").  // V1 = lut       (natural byte order)
		Raw("LXVB16X (R0)(R7), VS34").  // V2 = lomask
		Raw("LXVB16X (R0)(R8), VS35").  // V3 = four
		Raw("LXVB16X (R0)(R9), VS36").  // V4 = ctrlLo
		Raw("LXVB16X (R0)(R10), VS37"). // V5 = ctrlHi
		Raw("CMP R5, $0").Raw("BEQ edone").
		Raw("MOVD $0, R11"). // src byte offset
		Raw("MOVD $0, R12"). // dst byte offset
		Label("eloop").
		Raw("LXVB16X (R11)(R4), VS32"). // V0 = 16 src bytes in natural memory order
		Raw("VAND V0, V2, V6").         // V6 = low nibble
		Raw("VSRB V0, V3, V7").         // V7 = high nibble (byte >> 4)
		Raw("VPERM V1, V1, V7, V8").    // V8 = ASCII of high nibbles
		Raw("VPERM V1, V1, V6, V9").    // V9 = ASCII of low nibbles
		Raw("VPERM V8, V9, V4, V10").   // V10 = output bytes 0..15
		Raw("VPERM V8, V9, V5, V11").   // V11 = output bytes 16..31
		Raw("STXVB16X VS42, (R12)(R3)"). // store V10 (natural byte order)
		Raw("ADD $16, R12").
		Raw("STXVB16X VS43, (R12)(R3)"). // store V11
		Raw("ADD $16, R11").
		Raw("ADD $16, R12").
		Raw("ADD $-1, R5").
		Raw("CMP R5, $0").Raw("BNE eloop").
		Label("edone").Ret()
	f.Add(g.Func())

	if err := os.WriteFile("encode_ppc64le.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote encode_ppc64le.s")
}
