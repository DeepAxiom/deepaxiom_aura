package approvals

import (
	"testing"
	"time"
)

func TestApproveReleasesTheCaller(t *testing.T) {
	r := New()
	id, decision, release := r.Open(Pending{Question: "write to prod?"})
	defer release()

	go func() {
		if err := r.Resolve(id, true); err != nil {
			t.Errorf("resolve: %v", err)
		}
	}()
	select {
	case got := <-decision:
		if !got {
			t.Error("approve delivered a denial")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the caller was never released")
	}
}

func TestDenyReleasesTheCaller(t *testing.T) {
	r := New()
	id, decision, release := r.Open(Pending{Question: "q"})
	defer release()
	go func() { _ = r.Resolve(id, false) }()
	if got := <-decision; got {
		t.Error("deny delivered an approval")
	}
}

// The safety-critical default: an unanswered question is a refusal, never an
// approval, and the caller must not be left hanging on it either.
func TestExpiryDeniesRatherThanHanging(t *testing.T) {
	r := WithTTL(80 * time.Millisecond)
	_, decision, release := r.Open(Pending{Question: "q"})
	defer release()

	select {
	case got := <-decision:
		if got {
			t.Fatal("an expired question must never resolve as approved")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expiry never fired; the caller would hang forever")
	}
}

func TestResolvingTwiceIsReported(t *testing.T) {
	r := New()
	id, decision, release := r.Open(Pending{Question: "q"})
	defer release()
	if err := r.Resolve(id, true); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	<-decision
	if err := r.Resolve(id, true); err == nil {
		t.Error("the second resolve must report that the question is gone")
	}
}

func TestResolvingSomethingUnknownIsAnError(t *testing.T) {
	if err := New().Resolve("nope", true); err == nil {
		t.Error("an unknown id must not look like success")
	}
}

// A caller that gave up has to take its question with it, or an operator
// approves an effect that can no longer be delivered.
func TestReleaseRemovesTheQuestion(t *testing.T) {
	r := New()
	id, _, release := r.Open(Pending{Question: "q"})
	if len(r.List()) != 1 {
		t.Fatal("the question should be listed while it waits")
	}
	release()
	if len(r.List()) != 0 {
		t.Error("a released question must not stay listed")
	}
	if err := r.Resolve(id, true); err == nil {
		t.Error("a released question must not be resolvable")
	}
}

func TestListIsOldestFirst(t *testing.T) {
	r := New()
	_, _, r1 := r.Open(Pending{Question: "first"})
	defer r1()
	time.Sleep(5 * time.Millisecond)
	_, _, r2 := r.Open(Pending{Question: "second"})
	defer r2()
	time.Sleep(5 * time.Millisecond)
	_, _, r3 := r.Open(Pending{Question: "third"})
	defer r3()

	got := r.List()
	if len(got) != 3 {
		t.Fatalf("list = %d, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].Question != want {
			t.Errorf("position %d = %q, want %q", i, got[i].Question, want)
		}
	}
}

func TestGetReturnsTheDetailAnOperatorNeeds(t *testing.T) {
	r := New()
	id, _, release := r.Open(Pending{
		Question: "delete /etc?", Origin: "mcp", Tool: "delete_everything",
	})
	defer release()

	p, ok := r.Get(id)
	if !ok {
		t.Fatal("the question should be retrievable while it waits")
	}
	if p.Tool != "delete_everything" || p.Origin != "mcp" {
		t.Errorf("detail lost: %+v", p)
	}
	if p.Expires.Before(p.Requested) {
		t.Error("expiry must be after the request")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("an unknown id must not resolve")
	}
}

func TestOpenAssignsAnIDWhenNoneIsGiven(t *testing.T) {
	r := New()
	id, _, release := r.Open(Pending{Question: "q"})
	defer release()
	if id == "" {
		t.Error("a question with no id must still be answerable")
	}
}
