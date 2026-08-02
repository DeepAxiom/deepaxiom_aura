package gateway

import (
	"testing"
	"time"

	"aura/kernel/internal/approvals"
)

func approvalGateway(t *testing.T) (*Gateway, *approvals.Registry) {
	t.Helper()
	g, _ := testGateway(t)
	r := approvals.New()
	g.Approvals = r
	return g, r
}

func TestApprovalsListAndResolve(t *testing.T) {
	g, r := approvalGateway(t)
	h := g.Handler()

	code, body := do(t, h, "GET", "/v1/approvals", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	if list, _ := body["approvals"].([]any); len(list) != 0 {
		t.Errorf("a fresh node should have nothing waiting, got %v", list)
	}

	id, decision, release := r.Open(approvals.Pending{
		Question: "delete /etc?", Origin: "mcp", Tool: "rm"})
	defer release()

	code, body = do(t, h, "GET", "/v1/approvals", nil)
	list, _ := body["approvals"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list = %d %v", code, body)
	}
	first, _ := list[0].(map[string]any)
	if first["tool"] != "rm" || first["origin"] != "mcp" {
		t.Errorf("the operator needs the detail: %v", first)
	}

	code, _ = do(t, h, "POST", "/v1/approvals/"+id, map[string]any{"approve": true})
	if code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	select {
	case got := <-decision:
		if !got {
			t.Error("the route delivered a denial")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolving over HTTP never released the caller")
	}
}

func TestApprovalsRouteCanDeny(t *testing.T) {
	g, r := approvalGateway(t)
	id, decision, release := r.Open(approvals.Pending{Question: "q"})
	defer release()

	code, _ := do(t, g.Handler(), "POST", "/v1/approvals/"+id, map[string]any{"approve": false})
	if code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	if got := <-decision; got {
		t.Error("approve:false delivered an approval")
	}
}

// Omitting the field must not be read as either answer. Guessing here would
// mean approving or denying an effect nobody decided on.
func TestApprovalsRefusesAnAmbiguousBody(t *testing.T) {
	g, r := approvalGateway(t)
	id, _, release := r.Open(approvals.Pending{Question: "q"})
	defer release()

	code, body := do(t, g.Handler(), "POST", "/v1/approvals/"+id, map[string]any{})
	if code != 400 {
		t.Fatalf("an omitted decision must be a 400, got %d", code)
	}
	if body["error"] == nil {
		t.Error("the refusal should explain itself")
	}
	if len(r.List()) != 1 {
		t.Error("a refused request must leave the question waiting")
	}
}

func TestApprovalsUnknownIDIs404(t *testing.T) {
	g, _ := approvalGateway(t)
	code, _ := do(t, g.Handler(), "POST", "/v1/approvals/nope", map[string]any{"approve": true})
	if code != 404 {
		t.Errorf("unknown id = %d, want 404", code)
	}
}

// A node built without an approval queue answers 404 rather than panicking —
// the same nil-tolerance every other optional subsystem here has.
func TestApprovalsAbsentQueueIs404(t *testing.T) {
	g, _ := testGateway(t)
	h := g.Handler()
	if code, _ := do(t, h, "GET", "/v1/approvals", nil); code != 404 {
		t.Errorf("list without a queue = %d, want 404", code)
	}
	if code, _ := do(t, h, "POST", "/v1/approvals/x", map[string]any{"approve": true}); code != 404 {
		t.Errorf("resolve without a queue = %d, want 404", code)
	}
}
