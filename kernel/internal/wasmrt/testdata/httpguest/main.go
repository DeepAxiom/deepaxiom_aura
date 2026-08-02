// Command httpguest is a second wasmrt test fixture, compiled only by
// http_import_test.go (GOOS=wasip1 GOARCH=wasm) — separate from
// testdata/guest because this one, and only this one, needs to import
// env.http_fetch. It reads a request off stdin, calls the host import
// directly (the same //go:wasmimport contract a real skill author would
// use), and reports exactly what came back — a real guest exercising the
// real ABI, not a mock of it.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unsafe"
)

//go:wasmimport env http_fetch
func httpFetch(reqPtr, reqLen, respBufPtr, respBufCap uint32) int32

type request struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

// respBuf is the guest's own response buffer — a package-level array has a
// stable address in this program's linear memory for the guest to hand the
// host, which is exactly what lets this ABI skip a guest-exported allocator
// (see http_import.go's doc comment).
var respBuf [65536]byte

func ptrOf(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
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

	fetchReq, err := json.Marshal(map[string]string{"method": req.Method, "url": req.URL})
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal fetch request:", err)
		os.Exit(2)
	}

	n := httpFetch(ptrOf(fetchReq), uint32(len(fetchReq)), ptrOf(respBuf[:]), uint32(len(respBuf)))

	if n < 0 {
		out, _ := json.Marshal(map[string]any{"ok": false, "code": n})
		os.Stdout.Write(out)
		return
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "n": n, "text": string(respBuf[:n])})
	os.Stdout.Write(out)
}
