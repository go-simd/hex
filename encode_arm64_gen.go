//go:build ignore

// Command gen produces encode_arm64.s with go-asmgen: a vectorised lowercase
// hex (base16) encoder for arm64 NEON. Each input byte becomes two ASCII chars;
// per 16 input bytes the kernel emits 32 chars.
//
// Method (the amd64 SSSE3 algorithm ported to NEON):
//   - Split every byte into its high and low nibble: VUSHR $4 for the high
//     nibble, VAND with 0x0f for the low one.
//   - Map each nibble (0..15) to its lowercase ASCII digit via a single 16-byte
//     VTBL lookup of the table "0123456789abcdef".
//   - Interleave the two ASCII streams so each input byte's (hi,lo) chars land
//     adjacent. NEON does this for free with an interleaving store VST2.P:
//     VST2 writes dst[2i]=hiASCII[i], dst[2i+1]=loASCII[i], i.e.
//     hi0,lo0,hi1,lo1,... — exactly encoding/hex's byte order.
//
// So the per-block work is one load, one shift, one AND, two table lookups and
// one interleaving store. Run: go run encode_arm64_gen.go
package main

import (
	"fmt"
	"os"

	"github.com/go-asmgen/asmgen/abi"
	"github.com/go-asmgen/asmgen/arm64"
	"github.com/go-asmgen/asmgen/emit"
)

// hexLUT maps nibble 0..15 -> lowercase ASCII hex digit (the VTBL table).
var hexLUT = []byte("0123456789abcdef")

func main() {
	f := emit.NewFile("arm64")

	lut := f.Data("earmLut", hexLUT) // 16-byte VTBL nibble->ASCII table

	sig := abi.LayoutArgs(
		[]abi.Arg{abi.Slice("dst"), abi.Slice("src"), abi.Scalar("n", abi.Int64)},
		nil,
	)
	b := arm64.NewFunc("encodeBlocks", sig, 0)
	b.LoadArg("dst_base", "R0").
		LoadArg("src_base", "R1").
		LoadArg("n", "R2").
		// Load the 16-byte ASCII table into V8 and splat the low-nibble mask.
		Raw("MOVD $%s(SB), R3", lut).
		Raw("VLD1 (R3), [V8.B16]").
		Raw("VMOVI $15, V9.B16"). // 0x0f low-nibble mask
		Raw("CBZ R2, done").
		Label("loop").
		Raw("VLD1.P 16(R1), [V0.B16]").       // V0 = 16 source bytes, advance src
		Raw("VUSHR $4, V0.B16, V1.B16").      // V1 = high nibbles (0..15)
		Raw("VAND V9.B16, V0.B16, V2.B16").   // V2 = low nibbles (0..15)
		Raw("VTBL V1.B16, [V8.B16], V3.B16"). // V3 = ASCII of high nibbles
		Raw("VTBL V2.B16, [V8.B16], V4.B16"). // V4 = ASCII of low nibbles
		// Interleaving store: dst[2i]=hi, dst[2i+1]=lo -> hi0,lo0,hi1,lo1,...
		Raw("VST2.P [V3.B16, V4.B16], 32(R0)").
		Raw("SUB $1, R2").
		Raw("CBNZ R2, loop").
		Label("done").
		Ret()
	f.Add(b.Func())

	if err := os.WriteFile("encode_arm64.s", []byte(f.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote encode_arm64.s")
}
