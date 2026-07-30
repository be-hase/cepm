package extid

import (
	"encoding/base64"
	"testing"
)

func TestFromPath(t *testing.T) {
	// Expected values independently computed with:
	//   python3 -c 'import hashlib; h = hashlib.sha256(path.encode()).hexdigest()[:32];
	//               print("".join(chr(ord("a") + int(c, 16)) for c in h))'
	// The algorithm matches Chrome's crx_file::id_util::GenerateIdForPath.
	tests := []struct {
		path string
		want string
	}{
		{"/Users/alice/.cepm/repos/mytools/ext", "padbcojcikcnemkfdlfdmpndcijbikii"},
		{"/tmp/x", "cofgkkdgpfdilddleipdhopfbofennlf"},
	}
	for _, tt := range tests {
		got, err := FromPath(tt.path)
		if err != nil {
			t.Fatalf("FromPath(%q): %v", tt.path, err)
		}
		if got != tt.want {
			t.Errorf("FromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestFromPathRejectsRelative(t *testing.T) {
	if _, err := FromPath("relative/path"); err == nil {
		t.Error("FromPath should reject relative paths")
	}
}

func TestFromPathCleansTrailingSlash(t *testing.T) {
	a, _ := FromPath("/tmp/x")
	b, _ := FromPath("/tmp/x/")
	if a != b {
		t.Errorf("trailing slash changed ID: %q vs %q", a, b)
	}
}

func TestFromPublicKey(t *testing.T) {
	// A well-known public example: Chrome's docs use this key for the
	// "gnome-shell integration"-style stable-ID trick. We only assert the
	// format here plus determinism; the fixed helper-extension ID is asserted
	// in the helperext package against the committed key.
	key, err := base64.StdEncoding.DecodeString(
		"MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDcBHwzDvyBQ6bDppkIs9MP4ksKqCMyXQ/A52JivHZKh4YO/9vJsT3oaYhSpDCE9RPocOEQvwsHsFReW2nUEc6OLLyoCFFxIb7KkLGsmfakkut/fFdNJYh0xOTbSN8YvLWcqph09XAY2Y/f0AL7vfO1cuCqtkMt8hFrBGWxDdf9CQIDAQAB")
	if err != nil {
		t.Fatal(err)
	}
	got := FromPublicKey(key)
	if len(got) != 32 {
		t.Fatalf("ID length = %d, want 32", len(got))
	}
	for _, c := range got {
		if c < 'a' || c > 'p' {
			t.Fatalf("ID %q contains invalid character %q", got, c)
		}
	}
	if again := FromPublicKey(key); again != got {
		t.Error("FromPublicKey is not deterministic")
	}
}
