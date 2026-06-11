//go:build !amd64

package hex

// decodeSIMD has no SIMD kernel on this arch; the whole input goes to the
// scalar decoder (encoding/hex-equivalent) via the hex.go wrapper.
func decodeSIMD(dst, src []byte) (dstDone, srcDone int) { return 0, 0 }
