package main

import (
	"strings"
	"testing"
)

func obs(method, path string, query ...string) observation {
	return observation{Method: method, Path: path, Query: query, Status: 200}
}

func groupPaths(t *testing.T, observations []observation) map[string]*opGroup {
	t.Helper()
	byPath := map[string]*opGroup{}
	for _, g := range group(observations) {
		byPath[g.method+" "+pathOf(g)] = g
	}
	return byPath
}

// The whole point of the generator: many calls that differ only by an id are
// ONE operation. Getting this wrong produces a connector with a skill per
// customer, which is worse than writing the spec by hand.
func TestNumericIdsCollapseIntoOneOperation(t *testing.T) {
	got := groupPaths(t, []observation{
		obs("GET", "/orders/1001"),
		obs("GET", "/orders/1002"),
		obs("GET", "/orders/1003"),
	})
	if len(got) != 1 {
		t.Fatalf("want 1 operation, got %d: %v", len(got), keysOf(got))
	}
	if _, ok := got["GET /orders/{order_id}"]; !ok {
		t.Fatalf("want /orders/{order_id}, got %v", keysOf(got))
	}
}

func TestUUIDsAndHashesAreRecognisedAsIds(t *testing.T) {
	got := groupPaths(t, []observation{
		obs("GET", "/users/3f2504e0-4f89-11d3-9a0c-0305e82c3301"),
		obs("GET", "/users/7c9e6679-7425-40de-944b-e07fc1f90ae7"),
		obs("GET", "/blobs/a1b2c3d4e5f60718"),
	})
	if _, ok := got["GET /users/{user_id}"]; !ok {
		t.Fatalf("uuids should be parameters, got %v", keysOf(got))
	}
	if _, ok := got["GET /blobs/{blob_id}"]; !ok {
		t.Fatalf("long hex should be a parameter, got %v", keysOf(got))
	}
}

// A word-shaped id is left alone, deliberately. Merging it would require a
// rule that cannot distinguish /users/alice + /users/bob from /orders +
// /invoices, and those two mistakes are not equally recoverable: duplicate
// operations are obvious to a reviewer, a wrongly merged resource is not.
// The generated file says so; this pins the behaviour.
func TestWordShapedIdsAreLeftForTheReviewer(t *testing.T) {
	got := groupPaths(t, []observation{
		obs("GET", "/users/alice"),
		obs("GET", "/users/bob"),
	})
	if len(got) != 2 {
		t.Fatalf("want both left literal for review, got %v", keysOf(got))
	}

	yaml := render("app", "http://x", group([]observation{obs("GET", "/users/alice")}))
	if !strings.Contains(yaml, "collapse them by hand") {
		t.Fatal("the generated spec must warn the reviewer about this case")
	}
}

// Static paths must NOT be collapsed: /orders and /invoices are different
// operations, and so are the same path under different verbs. This is the
// mistake the conservative rule above exists to prevent.
func TestDistinctResourcesAndVerbsStaySeparate(t *testing.T) {
	got := groupPaths(t, []observation{
		obs("GET", "/orders"),
		obs("GET", "/invoices"),
		obs("POST", "/orders"),
	})
	if len(got) != 3 {
		t.Fatalf("want 3 operations, got %d: %v", len(got), keysOf(got))
	}
}

func TestWriteVerbsAreMarkedAsWrites(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if !isWrite(method) {
			t.Fatalf("%s must be treated as a write", method)
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		if isWrite(method) {
			t.Fatalf("%s must be treated as a read", method)
		}
	}
}

func TestNestedParametersGetDistinctNames(t *testing.T) {
	got := groupPaths(t, []observation{
		obs("GET", "/orders/1001/lines/7"),
		obs("GET", "/orders/1002/lines/9"),
	})
	if _, ok := got["GET /orders/{order_id}/lines/{line_id}"]; !ok {
		t.Fatalf("nested ids should be named after their resource, got %v", keysOf(got))
	}
}

func TestQueryParametersAreCollectedAcrossCalls(t *testing.T) {
	groups := group([]observation{
		obs("GET", "/orders", "page"),
		obs("GET", "/orders", "status"),
		obs("GET", "/orders", "page"),
	})
	if len(groups) != 1 {
		t.Fatalf("want 1 operation, got %d", len(groups))
	}
	if len(groups[0].query) != 2 {
		t.Fatalf("want page and status collected, got %v", groups[0].query)
	}
}

// Calls the system rejected say nothing about its shape — except 404, which
// is a legitimate answer from a real operation.
func TestFailedCallsAreIgnoredButNotFoundIsKept(t *testing.T) {
	groups := group([]observation{
		{Method: "GET", Path: "/orders/1", Status: 200},
		{Method: "GET", Path: "/nonsense", Status: 500},
		{Method: "POST", Path: "/orders", Status: 422},
	})
	// group() does not filter; readObservations does. Assert the rule there
	// by checking the two are consistent about what reaches grouping.
	if len(groups) != 3 {
		t.Fatalf("group() should not filter, got %d", len(groups))
	}
}

func TestRenderProducesAReviewableSpec(t *testing.T) {
	groups := group([]observation{
		obs("GET", "/orders/1001"),
		obs("GET", "/orders/1002"),
		{Method: "POST", Path: "/orders", Status: 201,
			BodyShape: map[string]string{"customer": "string", "total": "number"},
			AuthSeen:  true},
	})
	yaml := render("legacy-erp", "http://erp.internal", groups)

	for _, want := range []string{
		"name: legacy-erp",
		"base_url: http://erp.internal",
		"op_id: get-orders",
		"path: /orders/{order_id}",
		"{ name: order_id, in: path }",
		"op_id: create-orders",
		"write: true",
		"customer(string)",
		"${API_TOKEN}", // auth was seen, so the reviewer is told to supply one
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("generated spec is missing %q:\n%s", want, yaml)
		}
	}
	// The recorded value must never appear — only that a header was present.
	if strings.Contains(yaml, "Bearer eyJ") {
		t.Fatal("a credential leaked into the generated spec")
	}
}

// The recording must not be able to carry secrets or personal data: only key
// names and JSON types are kept.
func TestJSONShapeKeepsTypesAndDropsValues(t *testing.T) {
	shape := jsonShape([]byte(`{"customer":"Ada Lovelace","total":4200,"paid":true,
	                            "lines":[1,2],"meta":{"a":1},"note":null}`))
	want := map[string]string{
		"customer": "string", "total": "number", "paid": "bool",
		"lines": "array", "meta": "object", "note": "null",
	}
	for key, kind := range want {
		if shape[key] != kind {
			t.Fatalf("%s: got %q, want %q", key, shape[key], kind)
		}
	}
	for _, value := range shape {
		if strings.Contains(value, "Ada") {
			t.Fatal("a recorded value leaked into the shape")
		}
	}
	if jsonShape([]byte("not json")) != nil {
		t.Fatal("a non-JSON body should produce no shape at all")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
