package edge

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestSSEReaderDecodesEvents(t *testing.T) {
	stream := "event: hello\ndata: {\"server\":\"e1\"}\n\n" +
		": hb\n\n" +
		"event: exec\ndata: {\"execution_id\":\"ex_1\"}\n\n"
	r := newSSEReader(strings.NewReader(stream))

	ev, err := r.Next()
	if err != nil || ev.Name != "hello" || string(ev.Data) != `{"server":"e1"}` {
		t.Fatalf("hello frame = %+v, err=%v", ev, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "" || ev.Data != nil {
		t.Fatalf("heartbeat should decode to an empty event, got %+v err=%v", ev, err)
	}
	ev, err = r.Next()
	if err != nil || ev.Name != "exec" || string(ev.Data) != `{"execution_id":"ex_1"}` {
		t.Fatalf("exec frame = %+v, err=%v", ev, err)
	}
	if _, err = r.Next(); err != io.EOF {
		t.Fatalf("want EOF at the end of the stream, got %v", err)
	}
}

// A proxy may rewrite line endings, and unknown fields must not derail the
// parse.
func TestSSEReaderToleratesCRLFAndUnknownFields(t *testing.T) {
	stream := "id: 7\r\nevent: exec\r\ndata: {\"a\":1}\r\nretry: 1000\r\n\r\n"
	r := newSSEReader(strings.NewReader(stream))
	ev, err := r.Next()
	if err != nil || ev.Name != "exec" || string(ev.Data) != `{"a":1}` {
		t.Fatalf("frame = %+v, err=%v", ev, err)
	}
}

func TestSSEReaderJoinsMultilineData(t *testing.T) {
	r := newSSEReader(strings.NewReader("event: exec\ndata: one\ndata: two\n\n"))
	ev, err := r.Next()
	if err != nil || string(ev.Data) != "one\ntwo" {
		t.Fatalf("data = %q, err=%v", ev.Data, err)
	}
}

// The silence threshold must allow for one missed heartbeat plus slack, so a
// slow proxy is not mistaken for a dead main.
func TestSilenceThreshold(t *testing.T) {
	tr := &sseTransport{}
	if got := tr.silenceThreshold(45); got != 100*time.Second {
		t.Fatalf("threshold for a 45s heartbeat = %s, want 100s", got)
	}
	if got := tr.silenceThreshold(0); got != 100*time.Second {
		t.Fatalf("threshold with no advertised heartbeat = %s, want the 45s default", got)
	}
	tr.silenceOverride = time.Second
	if got := tr.silenceThreshold(45); got != time.Second {
		t.Fatalf("override ignored: %s", got)
	}
}
