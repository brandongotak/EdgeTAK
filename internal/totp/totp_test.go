package totp

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238 appendix B test vectors (SHA-1), truncated to 6 digits.
func TestRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		got, err := Code(secret, time.Unix(c.unix, 0))
		if err != nil || got != c.want {
			t.Errorf("Code(%d) = %s, %v; want %s", c.unix, got, err, c.want)
		}
	}
}

func TestVerify(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	cur, _ := Code(secret, now)
	prev, _ := Code(secret, now.Add(-30*time.Second))
	old, _ := Code(secret, now.Add(-90*time.Second))

	stepNow, ok := Verify(secret, cur, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := Verify(secret, cur, now, stepNow); ok {
		t.Fatal("a code was accepted twice")
	}
	if _, ok := Verify(secret, prev, now, 0); !ok {
		t.Fatal("previous step (clock drift) rejected")
	}
	if _, ok := Verify(secret, old, now, 0); ok && old != cur {
		t.Fatal("a code from 90 s ago was accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if u := URI("EdgeTAK", "admin", secret); u[:15] != "otpauth://totp/" {
		t.Fatalf("uri = %s", u)
	}
}
