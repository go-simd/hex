package hex

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"testing"

	tmthrgd "github.com/tmthrgd/go-hex"
)

var sizes = []int{0, 1, 2, 3, 7, 8, 15, 16, 17, 31, 32, 33, 63, 64, 65, 1000, 1024, 4096, 1 << 16}

func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// TestEncode checks EncodeToString and Encode are byte-identical to
// encoding/hex across edge sizes (empty, odd/even, sub-block, 1KiB, 64KiB).
func TestEncode(t *testing.T) {
	for _, n := range sizes {
		src := randBytes(n, int64(n)+1)
		if got, want := EncodeToString(src), hex.EncodeToString(src); got != want {
			t.Fatalf("EncodeToString n=%d:\n got=%q\nwant=%q", n, got, want)
		}
		dst := make([]byte, EncodedLen(len(src)))
		if got := Encode(dst, src); got != EncodedLen(n) {
			t.Fatalf("Encode n=%d returned %d, want %d", n, got, EncodedLen(n))
		}
		if want := hex.EncodeToString(src); string(dst) != want {
			t.Fatalf("Encode n=%d:\n got=%q\nwant=%q", n, dst, want)
		}
		// Competitor must be byte-identical too so the bench compares like for like.
		if got, want := tmthrgd.EncodeToString(src), hex.EncodeToString(src); got != want {
			t.Fatalf("tmthrgd n=%d:\n got=%q\nwant=%q", n, got, want)
		}
	}
}

// TestRoundTrip checks Decode(Encode(x)) == x.
func TestRoundTrip(t *testing.T) {
	for _, n := range sizes {
		src := randBytes(n, int64(n)*7+3)
		enc := EncodeToString(src)
		back, err := DecodeString(enc)
		if err != nil {
			t.Fatalf("DecodeString n=%d: %v", n, err)
		}
		if !bytes.Equal(back, src) {
			t.Fatalf("round-trip mismatch n=%d", n)
		}
	}
}

// TestDecodeMatchesStdlib checks Decode and DecodeString match encoding/hex
// exactly, including errors on invalid input (InvalidByteError offset, odd
// length / ErrLength).
func TestDecodeMatchesStdlib(t *testing.T) {
	cases := []string{
		"",
		"00",
		"deadbeef",
		"DEADBEEF",
		"DeAdBeEf",
		"0123456789abcdefABCDEF",
		"f",          // odd length
		"abc",        // odd length
		"xy",         // invalid bytes
		"0g",         // invalid second nibble
		"g0",         // invalid first nibble
		"00zz",       // valid then invalid
		"  ",         // spaces
		"deadbeefXX", // valid prefix, invalid tail
	}
	for _, s := range cases {
		gotB, gotErr := DecodeString(s)
		wantB, wantErr := hex.DecodeString(s)
		if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Fatalf("DecodeString(%q) err: got=%v want=%v", s, gotErr, wantErr)
		}
		if !bytes.Equal(gotB, wantB) {
			t.Fatalf("DecodeString(%q): got=%x want=%x", s, gotB, wantB)
		}

		// Decode into a buffer, comparing n and error.
		src := []byte(s)
		gd := make([]byte, DecodedLen(len(src)))
		wd := make([]byte, hex.DecodedLen(len(src)))
		gn, ge := Decode(gd, src)
		wn, we := hex.Decode(wd, src)
		if gn != wn || (ge == nil) != (we == nil) || (ge != nil && ge.Error() != we.Error()) {
			t.Fatalf("Decode(%q): got (%d,%v) want (%d,%v)", s, gn, ge, wn, we)
		}
		if !bytes.Equal(gd[:gn], wd[:wn]) {
			t.Fatalf("Decode(%q) bytes: got=%x want=%x", s, gd[:gn], wd[:wn])
		}
	}
}

func FuzzEncode(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("hello world"))
	f.Add(randBytes(33, 99))
	f.Fuzz(func(t *testing.T, src []byte) {
		if got, want := EncodeToString(src), hex.EncodeToString(src); got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
		// Encoded output must always round-trip.
		back, err := DecodeString(EncodeToString(src))
		if err != nil || !bytes.Equal(back, src) {
			t.Fatalf("round-trip failed: err=%v", err)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add("deadbeef")
	f.Add("DEADBEEF")
	f.Add("0g")
	f.Add("abc")
	f.Fuzz(func(t *testing.T, s string) {
		gotB, gotErr := DecodeString(s)
		wantB, wantErr := hex.DecodeString(s)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("DecodeString(%q) err presence: got=%v want=%v", s, gotErr, wantErr)
		}
		if gotErr != nil && gotErr.Error() != wantErr.Error() {
			t.Fatalf("DecodeString(%q) err: got=%v want=%v", s, gotErr, wantErr)
		}
		if !bytes.Equal(gotB, wantB) {
			t.Fatalf("DecodeString(%q): got=%x want=%x", s, gotB, wantB)
		}
	})
}

func benchData() []byte { return randBytes(1<<20, 2) }

func BenchmarkEncode(b *testing.B) {
	src := benchData()
	dst := make([]byte, EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Encode(dst, src)
	}
}

func BenchmarkEncodeStdlib(b *testing.B) {
	src := benchData()
	dst := make([]byte, EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hex.Encode(dst, src)
	}
}

// BenchmarkEncodeTmthrgd benchmarks github.com/tmthrgd/go-hex, a pure-Go SIMD
// (SSE/AVX, hand-written Plan9 asm) hex codec. Archived upstream Sep 2025.
func BenchmarkEncodeTmthrgd(b *testing.B) {
	src := benchData()
	dst := make([]byte, tmthrgd.EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmthrgd.Encode(dst, src)
	}
}

func BenchmarkDecode(b *testing.B) {
	enc := []byte(EncodeToString(benchData()))
	dst := make([]byte, DecodedLen(len(enc)))
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Decode(dst, enc)
	}
}

func BenchmarkDecodeStdlib(b *testing.B) {
	enc := []byte(hex.EncodeToString(benchData()))
	dst := make([]byte, hex.DecodedLen(len(enc)))
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hex.Decode(dst, enc)
	}
}

func BenchmarkDecodeTmthrgd(b *testing.B) {
	enc := []byte(EncodeToString(benchData()))
	dst := make([]byte, tmthrgd.DecodedLen(len(enc)))
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tmthrgd.Decode(dst, enc)
	}
}
