//go:build !amd64

package hex

// encodeSIMD has no SIMD kernel on this arch; the whole input goes to the
// standard library (encoding/hex via the hex.go wrapper).
func encodeSIMD(dst, src []byte) (srcDone, dstDone int) { return 0, 0 }
