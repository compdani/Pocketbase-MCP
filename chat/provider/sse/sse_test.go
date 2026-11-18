package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func readAll(t *testing.T, r *Reader) []Event {
	t.Helper()
	var out []Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

func TestFramesWithLF(t *testing.T) {
	in := "data: one\n\ndata: two\n\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Data != "one" || got[1].Data != "two" {
		t.Fatalf("events = %+v", got)
	}
}

func TestCRLFAndLoneCR(t *testing.T) {
	in := "data: a\r\n\r\ndata: b\r\rdata: c\r\r"
	got := readAll(t, NewReader(strings.NewReader(in)))
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i := range want {
		if got[i].Data != want[i] {
			t.Fatalf("event %d data = %q, want %q", i, got[i].Data, want[i])
		}
	}
}

func TestMultiLineDataAndComments(t *testing.T) {
	in := ": a comment\n" +
		"event: update\n" +
		"id: 7\n" +
		"data: first\n" +
		"data: second\n" +
		"retry: 100\n" +
		"\n" +
		": trailing comment\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 1 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Data != "first\nsecond" {
		t.Fatalf("data = %q", got[0].Data)
	}
	if got[0].Event != "update" || got[0].ID != "7" {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestCommentOnlyFrameIsSkipped(t *testing.T) {
	in := ": ping\n\ndata: x\n\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 1 || got[0].Data != "x" {
		t.Fatalf("events = %+v", got)
	}
}

func TestFieldWithoutSpaceAfterColon(t *testing.T) {
	in := "event:x\ndata:no-space\ndata:\n\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 1 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Event != "x" {
		t.Fatalf("event = %q", got[0].Event)
	}
	if got[0].Data != "no-space\n" {
		t.Fatalf("data = %q", got[0].Data)
	}
}

func TestFinalFrameWithoutBlankLine(t *testing.T) {
	in := "data: last\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 1 || got[0].Data != "last" {
		t.Fatalf("events = %+v", got)
	}

	// The frame is delivered once; the next call reports EOF.
	r := NewReader(strings.NewReader(in))
	if _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next = %v, want io.EOF", err)
	}
}

func TestFrameSplitAtEveryByte(t *testing.T) {
	in := "data: héllo wörld ✓\n\ndata: π\n\n"
	r := NewReader(iotest.OneByteReader(strings.NewReader(in)))
	got := readAll(t, r)
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Data != "héllo wörld ✓" {
		t.Fatalf("data = %q", got[0].Data)
	}
	if got[1].Data != "π" {
		t.Fatalf("data = %q", got[1].Data)
	}
}

func TestOversizeLine(t *testing.T) {
	in := "data: " + strings.Repeat("a", 64) + "\n\n"
	r := NewReaderSize(strings.NewReader(in), 16)
	if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Next = %v, want ErrTooLarge", err)
	}
}

func TestOversizeEventData(t *testing.T) {
	in := "data: 12345678\ndata: 12345678\ndata: 12345678\n\n"
	r := NewReaderSize(strings.NewReader(in), 20)
	if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Next = %v, want ErrTooLarge", err)
	}
}

func TestNulIDIgnored(t *testing.T) {
	in := "id: a\x00b\ndata: x\n\n"
	got := readAll(t, NewReader(strings.NewReader(in)))
	if len(got) != 1 || got[0].ID != "" {
		t.Fatalf("events = %+v", got)
	}
}

func TestEmptyStream(t *testing.T) {
	if _, err := NewReader(strings.NewReader("")).Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next = %v, want io.EOF", err)
	}
}
