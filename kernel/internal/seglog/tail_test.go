package seglog

import (
	"fmt"
	"testing"
)

func TestSessionTailReturnsTheMostRecentRecordsInOrder(t *testing.T) {
	l, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 50; i++ {
		for _, session := range []string{"a", "b"} {
			if err := l.Append(Entry{Session: session, MsgID: fmt.Sprintf("%s-%d", session, i),
				Kind: "data", TS: int64(i), Envelope: []byte(fmt.Sprintf(`{"n":%d}`, i))}); err != nil {
				t.Fatal(err)
			}
		}
	}
	tail, err := l.SessionTail("a", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 5 {
		t.Fatalf("tail of 5 returned %d records", len(tail))
	}
	for i, e := range tail {
		if want := fmt.Sprintf("a-%d", 45+i); e.MsgID != want {
			t.Fatalf("tail[%d] is %s, want %s", i, e.MsgID, want)
		}
	}
	// Asking for more than exists returns everything, same as Session.
	all, _ := l.SessionTail("a", 1000)
	head, _ := l.Session("a", 1000)
	if len(all) != 50 || len(head) != 50 || all[0].MsgID != head[0].MsgID {
		t.Fatalf("an oversized tail returned %d records", len(all))
	}
	if none, _ := l.SessionTail("nobody", 5); len(none) != 0 {
		t.Fatal("a session that was never written has a tail")
	}
}
