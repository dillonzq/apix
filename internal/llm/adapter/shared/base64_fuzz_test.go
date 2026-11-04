package shared

import (
	"bytes"
	std "encoding/base64"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
)

// FuzzBase64Parity pins the moego calls the media paths make to the
// standard library: ValidateString must accept exactly what DecodeString
// accepts, and decoding and encoding must give the same bytes.
func FuzzBase64Parity(f *testing.F) {
	for _, s := range []string{"", "aGk=", "aGk", "aGk=\n", "a\r\nGk=", "aG=k", "////", "-_-_", strings.Repeat("QUJD", 40), strings.Repeat("QUJD", 40) + "!"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// The repeated input reaches the SIMD block loops, which need more
		// than 64 characters.
		for _, s := range []string{in, strings.Repeat(in, 1+200/(len(in)+1))} {
			checkBase64Parity(t, s)
		}
	})
}

func checkBase64Parity(t *testing.T, s string) {
	t.Helper()
	want, wantErr := std.StdEncoding.DecodeString(s)
	n, err := base64.StdEncoding.ValidateString(s)
	if (err == nil) != (wantErr == nil) {
		t.Fatalf("ValidateString(%q) err = %v, stdlib %v", s, err, wantErr)
	}
	if err == nil && n != len(want) {
		t.Fatalf("ValidateString(%q) = %d, want %d", s, n, len(want))
	}
	got, err := base64.StdEncoding.DecodeString(s)
	if (err == nil) != (wantErr == nil) || (err == nil && !bytes.Equal(got, want)) {
		t.Fatalf("DecodeString(%q) = %q, %v; stdlib %q, %v", s, got, err, want, wantErr)
	}
	if enc := base64.StdEncoding.EncodeToString([]byte(s)); enc != std.StdEncoding.EncodeToString([]byte(s)) {
		t.Fatalf("EncodeToString(%q) = %q", s, enc)
	}
	if url := base64.EncodeDataURL("text/plain", []byte(s)); url != "data:text/plain;base64,"+std.StdEncoding.EncodeToString([]byte(s)) {
		t.Fatalf("EncodeDataURL(%q) = %q", s, url)
	}
}
