package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// cmdConnect — project an existing system's API as skills.
//
//	aura connect --openapi <url|file> [--name crm] [--base-url http://...]
//	             [--header "X-Api-Key: abc"] [--port 9080]
func cmdConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	spec := fs.String("openapi", "", "OpenAPI spec: URL or local file (JSON/YAML)")
	name := fs.String("name", "", "projection name (default: spec title)")
	baseURL := fs.String("base-url", "", "API base URL (default: servers[0].url)")
	port := fs.Int("port", 9080, "kernel port")
	var headers headerFlags
	fs.Var(&headers, "header", `extra header, repeatable: --header "K: V"`)
	_ = fs.Parse(args)

	if *spec == "" {
		fatal(fmt.Errorf("--openapi <url|file> is mandatory"))
	}
	raw, err := readSpec(*spec)
	if err != nil {
		fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"kind": "openapi", "name": *name, "base_url": *baseURL,
		"headers": headers.m, "spec": string(raw),
	})
	code, resp := postJSON(*port, "/v1/projections", body)
	if code != 201 {
		fatal(fmt.Errorf("kernel rejected the projection (%d): %s", code, resp["error"]))
	}

	ops, _ := resp["ops"].([]any)
	reads, writes := 0, 0
	for _, o := range ops {
		if op, ok := o.(map[string]any); ok {
			if w, _ := op["write"].(bool); w {
				writes++
			} else {
				reads++
			}
		}
	}
	fmt.Printf("projection %q registered: %d operations\n", resp["name"], len(ops))
	fmt.Printf("  %d reads ACTIVE (read-only)\n", reads)
	if writes > 0 {
		fmt.Printf("  %d writes DISABLED\n", writes)
		fmt.Printf("    enable one:  aura promote %v <op> --mode dry-run\n", resp["name"])
	}
	fmt.Printf("  details:       aura projections\n")
}

// cmdProjections — list projections and per-operation modes.
func cmdProjections(args []string) {
	fs := flag.NewFlagSet("projections", flag.ExitOnError)
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)

	code, resp := getJSON(*port, "/v1/projections")
	if code != 200 {
		fatal(fmt.Errorf("kernel error (%d): %s", code, resp["error"]))
	}
	list, _ := resp["projections"].([]any)
	if len(list) == 0 {
		fmt.Println("no projections — connect one: aura connect --openapi <url>")
		return
	}
	for _, p := range list {
		proj, ok := p.(map[string]any)
		if !ok {
			continue
		}
		fmt.Printf("\n%s  →  %s\n", proj["name"], proj["base_url"])
		ops, _ := proj["ops"].([]any)
		for _, o := range ops {
			op, ok := o.(map[string]any)
			if !ok {
				continue
			}
			mode := fmt.Sprint(op["mode"])
			mark := map[string]string{"live": "●", "dry-run": "◐", "disabled": "○"}[mode]
			fmt.Printf("  %s %-9s %-28v %-6v %s\n",
				mark, mode, op["op_id"], op["method"], op["path"])
		}
	}
	fmt.Println()
}

// cmdPromote — change one operation's mode (disabled → dry-run → live).
//
//	aura promote <projection> <op> --mode dry-run|live|disabled
func cmdPromote(args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	mode := fs.String("mode", "dry-run", "live | dry-run | disabled")
	port := fs.Int("port", 9080, "kernel port")
	_ = fs.Parse(args)
	rest := fs.Args()
	// Go's flag stops at the first positional: re-parse what follows them
	// so `aura promote <proj> <op> --mode live` works naturally.
	if len(rest) > 2 {
		_ = fs.Parse(rest[2:])
		rest = rest[:2]
	}
	if len(rest) != 2 {
		fatal(fmt.Errorf("usage: aura promote <projection> <op> --mode dry-run|live|disabled"))
	}
	body, _ := json.Marshal(map[string]string{"op": rest[1], "mode": *mode})
	code, resp := postJSON(*port, "/v1/projections/"+rest[0]+"/promote", body)
	if code != 200 {
		fatal(fmt.Errorf("promote failed (%d): %s", code, resp["error"]))
	}
	fmt.Printf("%s/%s → %s\n", rest[0], rest[1], *mode)
	if *mode == "live" {
		fmt.Println("  this operation will perform REAL WRITES on the target system")
	}
}

// ── helpers ──────────────────────────────────────────────────────

type headerFlags struct{ m map[string]string }

func (h *headerFlags) String() string { return "" }
func (h *headerFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header must be \"Key: Value\"")
	}
	if h.m == nil {
		h.m = map[string]string{}
	}
	h.m[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

func readSpec(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := http.Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetch spec: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("fetch spec: HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	}
	return os.ReadFile(src)
}

func postJSON(port int, path string, body []byte) (int, map[string]any) {
	code, data := newNodeClient(port).raw(http.MethodPost, path, body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return code, out
}

func getJSON(port int, path string) (int, map[string]any) {
	code, data := newNodeClient(port).raw(http.MethodGet, path, nil)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return code, out
}
