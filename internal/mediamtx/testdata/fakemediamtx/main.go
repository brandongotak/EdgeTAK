// A stand-in for MediaMTX in tests: serves /v3/info with its version on the
// config file's apiAddress, or exits at once when built as a broken release.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

var (
	version = "v0.0.0"
	broken  = ""
)

func main() {
	if broken != "" {
		fmt.Println("ERR fake broken release")
		os.Exit(1)
	}
	conf, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	addr := ""
	for _, line := range strings.Split(string(conf), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "apiAddress:"); ok {
			addr = strings.TrimSpace(v)
		}
	}
	http.HandleFunc("/v3/info", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"started":"2026-01-01T00:00:00Z"}`, version)
	})
	fmt.Println("INF fake MediaMTX", version, "listening on", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
}
