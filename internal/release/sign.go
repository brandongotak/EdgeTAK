package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Signed releases: each release's SHA256SUMS is signed with an Ed25519 key
// kept offline (never on GitHub), and the signature published beside it as
// SHA256SUMS.sig. The console updater checks it with the EdgeTAK already
// installed before trusting the checksums, so a tampered release (a stolen
// GitHub account, a compromised build) can't be installed.
//
// The signature file is two lines:
//
//	key: <key id>
//	sig: <base64 Ed25519 signature of the SHA256SUMS bytes>

// PublicKeys are the release signing keys EdgeTAK trusts, base64. A new key
// is added here a release before releases are signed with it.
var PublicKeys = []string{
	"v+MkD6SE2SRz621Gav3m5TD7LL6Ub95ZbefV4EWrrok=", // 55c8667e, from 2026-10-23
}

// KeyID names a public key: the first 8 hex digits of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:4])
}

// Sign makes the signature file for sums.
func Sign(priv ed25519.PrivateKey, sums []byte) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	return fmt.Appendf(nil, "key: %s\nsig: %s\n", KeyID(pub), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)))
}

// Verify checks sig against sums with the trusted keys (trusted: nil means
// PublicKeys), returning the key id that signed it.
func Verify(sums, sig []byte, trusted []string) (string, error) {
	if trusted == nil {
		trusted = PublicKeys
	}
	var keyID, sigB64 string
	for _, line := range strings.Split(string(bytes.TrimSpace(sig)), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "key":
			keyID = strings.TrimSpace(v)
		case "sig":
			sigB64 = strings.TrimSpace(v)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", errors.New("not an EdgeTAK release signature")
	}
	for _, k := range trusted {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(b) != ed25519.PublicKeySize {
			continue
		}
		pub := ed25519.PublicKey(b)
		if keyID != "" && KeyID(pub) != keyID {
			continue
		}
		if ed25519.Verify(pub, sums, raw) {
			return KeyID(pub), nil
		}
	}
	if keyID != "" && !containsKeyID(trusted, keyID) {
		return "", fmt.Errorf("signed with key %s, which this EdgeTAK doesn't trust", keyID)
	}
	return "", errors.New("the signature doesn't match: the release has been altered")
}

func containsKeyID(keys []string, id string) bool {
	for _, k := range keys {
		if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == ed25519.PublicKeySize && KeyID(b) == id {
			return true
		}
	}
	return false
}

// ParsePrivateKey reads a signing key file: comment lines (#) and the
// base64 seed.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seed, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("not an EdgeTAK signing key")
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	return nil, errors.New("not an EdgeTAK signing key")
}
