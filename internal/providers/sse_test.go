package providers

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReader(t *testing.T) {
	in := ": keep-alive\n\nevent: message_start\ndata: {\"a\":1}\n\ndata: line1\r\ndata: line2\n\ndata: tail"
	r := newSSEReader(strings.NewReader(in))

	ev, data, err := r.next()
	if err != nil || ev != "message_start" || string(data) != `{"a":1}` {
		t.Fatalf("1: %q %q %v", ev, data, err)
	}
	ev, data, err = r.next()
	if err != nil || ev != "" || string(data) != "line1\nline2" {
		t.Fatalf("2: %q %q %v", ev, data, err)
	}
	// Truncated final event (no blank line): reported as unexpected EOF.
	if _, data, err = r.next(); !errors.Is(err, io.ErrUnexpectedEOF) || string(data) != "tail" {
		t.Fatalf("3: %q %v", data, err)
	}
	if _, _, err = newSSEReader(strings.NewReader("data: x\n\n")).next(); err != nil {
		t.Fatal(err)
	}
	r2 := newSSEReader(strings.NewReader("data: x\n\n"))
	_, _, _ = r2.next()
	if _, _, err := r2.next(); err != io.EOF {
		t.Fatalf("clean end: %v", err)
	}
}
