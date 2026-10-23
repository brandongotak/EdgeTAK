package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	trusted := []string{base64.StdEncoding.EncodeToString(pub)}
	sums := []byte("abc123  edgetak-installer.sh\n")
	sig := Sign(priv, sums)
	if id, err := Verify(sums, sig, trusted); err != nil || id != KeyID(pub) {
		t.Fatalf("verify = %q %v", id, err)
	}
	// Altered checksums, a signature from another key, garbage.
	if _, err := Verify([]byte("evil  edgetak-installer.sh\n"), sig, trusted); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Errorf("altered sums = %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(sums, Sign(other, sums), trusted); err == nil || !strings.Contains(err.Error(), "doesn't trust") {
		t.Errorf("untrusted key = %v", err)
	}
	if _, err := Verify(sums, []byte("hello"), trusted); err == nil {
		t.Error("garbage accepted")
	}
	// The key file round-trips.
	file := "# comment\n" + base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	k, err := ParsePrivateKey([]byte(file))
	if err != nil || !k.Equal(priv) {
		t.Errorf("parse key = %v", err)
	}
	// The built-in key is well formed.
	for _, k := range PublicKeys {
		if b, err := base64.StdEncoding.DecodeString(k); err != nil || len(b) != ed25519.PublicKeySize {
			t.Errorf("bad built-in key %q", k)
		}
	}
}
