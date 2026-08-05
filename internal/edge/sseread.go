package edge

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one decoded server-sent event. A comment frame (the
// heartbeat) decodes to the zero value: no name, no data. Callers treat it
// as proof the stream is alive.
type sseEvent struct {
	Name string
	Data []byte
}

// sseReader decodes the slice of the text/event-stream grammar this
// protocol uses: `event:` and `data:` fields, `:` comments, and a blank line
// to dispatch. Field values may be preceded by one optional space, and CRLF
// line endings are tolerated.
type sseReader struct {
	br   *bufio.Reader
	name string
	data []byte
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReader(r)}
}

// Next reads until the next complete event or comment.
func (r *sseReader) Next() (sseEvent, error) {
	for {
		line, err := r.br.ReadString('\n')
		if err != nil {
			return sseEvent{}, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "":
			if r.name == "" && r.data == nil {
				continue // stray blank line between frames
			}
			ev := sseEvent{Name: r.name, Data: r.data}
			r.name, r.data = "", nil
			return ev, nil
		case strings.HasPrefix(line, ":"):
			return sseEvent{}, nil // heartbeat
		case strings.HasPrefix(line, "event:"):
			r.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if r.data != nil {
				r.data = append(r.data, '\n')
			} else {
				r.data = []byte{}
			}
			r.data = append(r.data, chunk...)
		}
		// Unknown fields (id:, retry:) are ignored, as the spec requires.
	}
}
