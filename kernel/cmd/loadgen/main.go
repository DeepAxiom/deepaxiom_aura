package main

// A load harness where nothing except the kernel can be the bottleneck.
//
// Earlier attempts measured the harness: a Python asyncio client tops out near
// 1,900 round-trips/sec, and — worse — the echo skill they drove was also a
// single-threaded Python process that every message had to pass through twice.
// Both ends are Go here, the skill answers concurrently, and the store alone is
// known to sustain ~17,500 writes/sec, so whatever this finds is the kernel.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ── the skill side ──────────────────────────────────────────────────

func runSkill(port int, cap string, ready chan<- struct{}) {
	url := fmt.Sprintf("ws://127.0.0.1:%d/ws/skill", port)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		panic(err)
	}
	m := map[string]any{
		"id": "example/logical/loadecho", "version": "1.0.0", "protocol": "1",
		"name": "load echo", "description": "echoes, fast", "capability": cap,
		"type": "logical", "format": "source",
		"ports": map[string]any{
			"ingress": []map[string]any{{"name": "text_in", "schema": "std/text@1"}},
			"egress":  []map[string]any{{"name": "text_out", "schema": "std/text@1"}},
		},
	}
	payload, _ := json.Marshal(m)
	_ = conn.WriteJSON(map[string]any{
		"v": "1", "id": newID(), "kind": "register", "payload": json.RawMessage(payload)})

	var ack map[string]any
	if err := conn.ReadJSON(&ack); err != nil {
		panic(err)
	}
	close(ready)

	var writeMu sync.Mutex
	var seq uint64
	for {
		var env map[string]any
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		if env["kind"] != "data" {
			continue
		}
		go func(env map[string]any) {
			n := atomic.AddUint64(&seq, 1)
			reply := map[string]any{
				"v": "1", "id": newID(), "cause_id": env["id"],
				"session": env["session"], "node": env["node"],
				"port": "text_out", "seq": n,
				"idem":   fmt.Sprintf("%v:%v:text_out:%d", env["idem"], env["node"], n),
				"schema": "std/text@1", "kind": "data",
				"payload": map[string]any{"text": "ok", "final": true},
			}
			writeMu.Lock()
			_ = conn.WriteJSON(reply)
			writeMu.Unlock()
		}(env)
	}
}

var idCounter uint64

func newID() string {
	return fmt.Sprintf("%026X", atomic.AddUint64(&idCounter, 1)+uint64(time.Now().UnixNano()))
}

// ── the client side ─────────────────────────────────────────────────

type result struct {
	lat []float64
	err string
}

func session(port int, graph string, idx, msgs int, out chan<- result, opened *int64) {
	url := fmt.Sprintf("ws://127.0.0.1:%d/v1/stream?graph=%s&session=s-%d-%d",
		port, graph, idx, time.Now().UnixNano()%1000000)
	d := websocket.Dialer{HandshakeTimeout: 60 * time.Second}
	conn, _, err := d.Dial(url, nil)
	if err != nil {
		out <- result{err: "dial: " + err.Error()}
		return
	}
	defer conn.Close()
	atomic.AddInt64(opened, 1)

	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	var hello map[string]any
	if err := conn.ReadJSON(&hello); err != nil {
		out <- result{err: "hello: " + err.Error()}
		return
	}
	lat := make([]float64, 0, msgs)
	for i := 0; i < msgs; i++ {
		t0 := time.Now()
		if err := conn.WriteJSON(map[string]string{"text": "m"}); err != nil {
			out <- result{lat: lat, err: "write: " + err.Error()}
			return
		}
		for {
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			var env map[string]any
			if err := conn.ReadJSON(&env); err != nil {
				out <- result{lat: lat, err: "read: " + err.Error()}
				return
			}
			if env["kind"] == "data" {
				break
			}
		}
		lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
	}
	out <- result{lat: lat}
}

func main() {
	port := flag.Int("port", 9160, "kernel port")
	sessions := flag.Int("sessions", 100, "concurrent sessions")
	msgs := flag.Int("msgs", 20, "round-trips per session")
	ramp := flag.Duration("ramp", 0, "stagger dials across this window (0 = thundering herd)")
	replicas := flag.Int("replicas", 1, "skill replicas serving the capability")
	flag.Parse()

	stamp := time.Now().UnixNano() % 1000000
	cap := fmt.Sprintf("logical.load%d", stamp)
	gid := fmt.Sprintf("load-%d", stamp)

	for i := 0; i < *replicas; i++ {
		ready := make(chan struct{})
		go runSkill(*port, cap, ready)
		<-ready
	}

	g, _ := json.Marshal(map[string]any{
		"ir": "1", "graph_id": gid, "origin": map[string]string{"kind": "declared"},
		"nodes": []map[string]any{{"ref": "e", "resolve": cap}},
		"edges": []map[string]any{
			{"from": "client.text_out", "to": "e.text_in"},
			{"from": "e.text_out", "to": "client.text_in"},
		},
	})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/graphs", *port),
		"application/json", bytes.NewReader(g))
	if err != nil {
		panic(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		fmt.Printf("graph registration: %d\n", resp.StatusCode)
		return
	}

	out := make(chan result, *sessions)
	var opened int64
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *sessions; i++ {
		wg.Add(1)
		if *ramp > 0 {
			time.Sleep(*ramp / time.Duration(*sessions))
		}
		go func(i int) { defer wg.Done(); session(*port, gid, i, *msgs, out, &opened) }(i)
	}
	wg.Wait()
	close(out)
	wall := time.Since(start)

	var all []float64
	errs := map[string]int{}
	for r := range out {
		all = append(all, r.lat...)
		if r.err != "" {
			errs[r.err]++
		}
	}
	sort.Float64s(all)
	fmt.Printf("\n  sessions              %d opened / %d attempted\n", opened, *sessions)
	fmt.Printf("  round-trips           %d / %d\n", len(all), *sessions**msgs)
	for e, c := range errs {
		fmt.Printf("      x%-5d %s\n", c, e)
		break
	}
	if len(all) == 0 {
		return
	}
	fmt.Printf("  aggregate throughput  %8.0f msg/s\n", float64(len(all))/wall.Seconds())
	fmt.Printf("  p50                   %8.2f ms\n", all[len(all)/2])
	fmt.Printf("  p99                   %8.2f ms\n", all[int(float64(len(all))*0.99)])
}
