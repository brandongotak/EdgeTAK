// Package totp implements time-based one-time passwords (RFC 6238) as used
// by authenticator apps: HMAC-SHA1, 30-second steps, 6 digits.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	step   = 30 * time.Second
	digits = 6
	// Window accepts the previous and next code too, for clock drift.
	window = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit secret, base32-encoded.
func NewSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// URI is the otpauth:// link authenticator apps scan as a QR code.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Code is the code for the time step containing t.
func Code(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	return code(key, t.Unix()/int64(step.Seconds())), nil
}

func code(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, v%1000000)
}

// Verify checks code against secret at time t. It returns the matching time
// step, which must be greater than lastStep so a code cannot be used twice.
func Verify(secret, input string, t time.Time, lastStep int64) (int64, bool) {
	input = strings.ReplaceAll(strings.TrimSpace(input), " ", "")
	if len(input) != digits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	now := t.Unix() / int64(step.Seconds())
	for d := -window; d <= window; d++ {
		c := now + int64(d)
		if c <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(code(key, c)), []byte(input)) == 1 {
			return c, true
		}
	}
	return 0, false
}
