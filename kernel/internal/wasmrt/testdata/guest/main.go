// Command guest is a tiny fixture, compiled only by wasmrt's own tests
// (GOOS=wasip1 GOARCH=wasm) — never part of the kernel module's own build,
// which is why it lives under testdata/ (go build/vet skip that directory
// automatically). It exists to give runtime_test.go a real, compiled WASI
// command to run through the sandbox instead of a mock — the sandbox is
// what this package's tests exist to prove, and a mock guest would prove
// nothing about it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type request struct {
	Mode string `json:"mode"`
	Text string `json:"text"`
	Path string `json:"path"`
}

type response struct {
	OK   bool   `json:"ok"`
	Text string `json:"text,omitempty"`
}

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(2)
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintln(os.Stderr, "bad request json:", err)
		os.Exit(2)
	}

	switch req.Mode {
	case "echo":
		out, _ := json.Marshal(response{OK: true, Text: upper(req.Text)})
		os.Stdout.Write(out)

	case "fail":
		fmt.Fprintln(os.Stderr, "guest failed on purpose")
		os.Exit(3)

	case "fswrite":
		if err := os.WriteFile(req.Path, []byte(req.Text), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		out, _ := json.Marshal(response{OK: true})
		os.Stdout.Write(out)

	case "fsread":
		got, err := os.ReadFile(req.Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		out, _ := json.Marshal(response{OK: true, Text: string(got)})
		os.Stdout.Write(out)

	case "loop":
		// A busy loop, not a blocking primitive: this is exactly what
		// wazero's compiled-mode cancellation checks (WithCloseOnContextDone)
		// are designed to interrupt at a loop back-edge.
		for i := 0; ; i++ {
			_ = i
		}

	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", req.Mode)
		os.Exit(2)
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}
