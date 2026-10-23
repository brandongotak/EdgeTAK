package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/brandongotak/EdgeTAK/internal/release"
)

// runVerifyRelease is `edgetak verify-release SHA256SUMS SHA256SUMS.sig`:
// exit 0 when the checksums carry a valid signature from a key this
// EdgeTAK trusts. The updater runs it before installing a release.
func runVerifyRelease(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: edgetak verify-release SHA256SUMS SHA256SUMS.sig")
		return 2
	}
	sums, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak verify-release:", err)
		return 1
	}
	sig, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak verify-release: no signature:", err)
		return 1
	}
	id, err := release.Verify(sums, sig, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak verify-release:", err)
		return 1
	}
	fmt.Printf("good signature from EdgeTAK release key %s\n", id)
	return 0
}

// runSignRelease is `edgetak sign-release -key FILE SHA256SUMS`: writes
// SHA256SUMS.sig. Run by the maintainer on the machine holding the key.
func runSignRelease(args []string) int {
	fs := flag.NewFlagSet("edgetak sign-release", flag.ContinueOnError)
	keyFile := fs.String("key", "", "the release signing key `file`")
	if err := fs.Parse(args); err != nil || *keyFile == "" || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: edgetak sign-release -key FILE SHA256SUMS")
		return 2
	}
	keyData, err := os.ReadFile(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak sign-release:", err)
		return 1
	}
	priv, err := release.ParsePrivateKey(keyData)
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak sign-release:", err)
		return 1
	}
	sums, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak sign-release:", err)
		return 1
	}
	sig := release.Sign(priv, sums)
	// Never publish a signature this EdgeTAK wouldn't accept.
	id, err := release.Verify(sums, sig, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetak sign-release: this key isn't one EdgeTAK trusts:", err)
		return 1
	}
	if err := os.WriteFile(fs.Arg(0)+".sig", sig, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "edgetak sign-release:", err)
		return 1
	}
	fmt.Printf("signed %s with key %s: %s.sig\n", fs.Arg(0), id, fs.Arg(0))
	return 0
}
