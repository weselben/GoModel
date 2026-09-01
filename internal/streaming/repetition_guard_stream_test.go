package streaming

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

type recordingReadCloser struct {
	io.Reader
	closeCount int
	closeErr   error
}

func (r *recordingReadCloser) Close() error {
	r.closeCount++
	return r.closeErr
}

func newSource(data string) *recordingReadCloser {
	return &recordingReadCloser{Reader: strings.NewReader(data)}
}

func chatEvent(content string) string {
	return `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

func TestRepetitionGuardStream_DisabledReturnsOriginal(t *testing.T) {
	for _, limit := range []int{0, -5} {
		data := chatEvent("hello") + chatEvent("world") + doneEvent()
		src := newSource(data)
		stream := NewRepetitionGuardStream(src, limit)
		if stream != src {
			t.Fatalf("limit=%d: expected original source, got wrapper", limit)
		}
		out, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("limit=%d: ReadAll error: %v", limit, err)
		}
		if string(out) != data {
			t.Fatalf("limit=%d: passthrough mismatch\nwant: %q\ngot:  %q", limit, data, string(out))
		}
	}
}

func TestRepetitionGuardStream_SingleTokenRepetition(t *testing.T) {
	limit := 3
	var input bytes.Buffer
	input.WriteString(chatEvent("x"))
	for i := 0; i < 10; i++ {
		input.WriteString(chatEvent("a"))
	}
	input.WriteString(chatEvent("tail"))
	input.WriteString(doneEvent())

	src := newSource(input.String())
	stream := NewRepetitionGuardStream(src, limit)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src.closeCount == 0 {
		t.Fatalf("upstream source was not closed on repetition")
	}

	want := chatEvent("x") + chatEvent("a") + `data: {"choices":[{"delta":{"content":""}}]}` + "\n\n" + doneEvent()
	if string(out) != want {
		t.Fatalf("single-token output mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
}

func TestRepetitionGuardStream_TokenChainRepetition(t *testing.T) {
	limit := 2
	var input bytes.Buffer
	input.WriteString(chatEvent("hello "))
	for i := 0; i < 5; i++ {
		input.WriteString(chatEvent("ab"))
	}
	input.WriteString(doneEvent())

	src := newSource(input.String())
	stream := NewRepetitionGuardStream(src, limit)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src.closeCount == 0 {
		t.Fatalf("upstream source was not closed on chain repetition")
	}

	want := chatEvent("hello ") + chatEvent("ab") + `data: {"choices":[{"delta":{"content":""}}]}` + "\n\n" + doneEvent()
	if string(out) != want {
		t.Fatalf("chain output mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
}

func TestRepetitionGuardStream_NonRepeatingPassesThrough(t *testing.T) {
	limit := 3
	data := chatEvent("the") + chatEvent(" quick") + chatEvent(" brown") + chatEvent(" fox") + doneEvent()
	src := newSource(data)
	stream := NewRepetitionGuardStream(src, limit)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != data {
		t.Fatalf("non-repeating passthrough mismatch\nwant: %q\ngot:  %q", data, string(out))
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source close on stream Close, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_EmitsDoneOnTrigger(t *testing.T) {
	limit := 2
	data := chatEvent("hi") + chatEvent("hi") + chatEvent("hi")
	src := newSource(data)
	stream := NewRepetitionGuardStream(src, limit)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.HasSuffix(out, []byte(doneEvent())) {
		t.Fatalf("expected final [DONE], got %q", string(out))
	}
}

func TestDetectRun(t *testing.T) {
	cases := []struct {
		text     string
		limit    int
		wantUnit int
		wantKeep int // keepEnd byte offset
	}{
		{"preaaaa", 3, 1, 4},       // single token stutter after a prefix
		{"preababab", 2, 2, 5},     // chain "ab" after a prefix
		{"prexyzxyzxyz", 3, 3, 6},  // chain "xyz" after a prefix
		{"hihi hellohellohello", 3, 5, 10}, // chain "hello" after a prefix
	}

	for _, tc := range cases {
		unitLen, runStart, ok := detectRun([]byte(tc.text), tc.limit, defaultMaxUnitBytes)
		if !ok {
			t.Fatalf("%q limit=%d: expected repetition", tc.text, tc.limit)
		}
		if unitLen != tc.wantUnit || runStart+unitLen != tc.wantKeep {
			t.Fatalf("%q limit=%d: want unit=%d keepEnd=%d, got unit=%d keepEnd=%d",
				tc.text, tc.limit, tc.wantUnit, tc.wantKeep, unitLen, runStart+unitLen)
		}
	}
}

func doneEvent() string {
	return "data: [DONE]\n\n"
}
