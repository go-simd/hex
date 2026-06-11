//go:build ignore

// Command gen produces encode_s390x.s with go-asmgen: a vectorised lowercase hex
// (base16) encoder for s390x (IBM Z, vector facility, BIG-ENDIAN). Each input
// byte becomes two ASCII chars; 16 input bytes -> 32 hex chars per loop.
//
// Method (mirrors the amd64 PSHUFB approach with VPERM as the table lookup):
//   - Load 16 source bytes with VL. On big-endian s390x, VL puts the first
//     memory byte into lane 0 (the high-order/leftmost lane) — i.e. lanes are in
//     natural memory order, and VST mirrors it, so a load/permute/store round-
//     trips byte-for-byte without an endian fix-up.
//   - Low nibble:  VN src, lomask(0x0f).
//   - High nibble: VESRLB $4, src — a per-element logical shift right by 4 gives
//     the high nibble (0..15).
//   - Map each nibble to its ASCII digit with VPERM using the 16-entry table
//     "0123456789abcdef" as both permute sources; indices are 0..15 so the
//     lookup stays within the first source.
//   - Interleave so output is hi0,lo0,hi1,lo1,... with two VPERM passes whose
//     control vectors pick alternating bytes from the ASCII-hi/ASCII-lo vectors,
//     then VST the two halves.
//
// BIG-ENDIAN NOTE: hex output is a byte stream with each input byte's high
// nibble first (hi0,lo0,...). Because VL lane 0 == first memory byte == leftmost
// lane on s390x, the same natural-order control vectors used on a natural-order
// little-endian load are correct here; the per-byte high-then-low ordering is
// fixed by the control vectors, not by endianness. The position-dependent
// FuzzEncode test (encode 0x12 0x34 -> "1234") is the gate.
//
// Run: GOWORK=off go run encode_s390x_gen.go
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

var hexLUT = []byte("0123456789abcdef")

func sig() abi.Signature {
	return abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		nil,
	)
}

func main() {
	f := emit.NewFile("s390x")

	lut := f.Data("hexlut_z", hexLUT)
	lo4 := f.Data("lomask_z", repByte(0x0f, 16))
	ctrlLo := f.Data("ctrlLo_z", []byte{0, 16, 1, 17, 2, 18, 3, 19, 4, 20, 5, 21, 6, 22, 7, 23})
	ctrlHi := f.Data("ctrlHi_z", []byte{8, 24, 9, 25, 10, 26, 11, 27, 12, 28, 13, 29, 14, 30, 15, 31})

	g := s390x.NewFunc("encodeBlocksVX", sig(), 0)
	g.LoadArg("dst_base", "R1").LoadArg("src_base", "R2").LoadArg("n", "R3").
		// Load constants into vector registers.
		Raw("MOVD $%s+0(SB), R5", lut).Raw("VL (R5), V1").    // V1 = lut
		Raw("MOVD $%s+0(SB), R5", lo4).Raw("VL (R5), V2").    // V2 = lomask
		Raw("MOVD $%s+0(SB), R5", ctrlLo).Raw("VL (R5), V4"). // V4 = ctrlLo
		Raw("MOVD $%s+0(SB), R5", ctrlHi).Raw("VL (R5), V5"). // V5 = ctrlHi
		Raw("CMPBEQ R3, $0, edone").
		Label("eloop").
		Raw("VL (R2), V0").             // V0 = 16 src bytes (lane 0 = first byte)
		Raw("VN V0, V2, V6").           // V6 = low nibble
		Raw("VESRLB $4, V0, V7").       // V7 = high nibble (byte >> 4)
		Raw("VPERM V1, V1, V7, V8").    // V8 = ASCII of high nibbles
		Raw("VPERM V1, V1, V6, V9").    // V9 = ASCII of low nibbles
		Raw("VPERM V8, V9, V4, V10").   // V10 = output bytes 0..15
		Raw("VPERM V8, V9, V5, V11").   // V11 = output bytes 16..31
		Raw("VST V10, (R1)").           // store first 16 chars
		Raw("VST V11, 16(R1)").         // store next 16 chars
		Raw("ADD $16, R2").
		Raw("ADD $32, R1").
		Raw("ADD $-1, R3").
		Raw("CMPBNE R3, $0, eloop").
		Label("edone").Ret()
	f.Add(g.Func())

	if err := os.WriteFile("encode_s390x.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote encode_s390x.s")
}
