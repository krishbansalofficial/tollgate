package providers

import (
	"bufio"
	"bytes"
	"io"
)

// maxSSELine bounds one SSE line and maxSSEEvent one event (which may span
// many data: lines). A misbehaving upstream must not grow our memory
// without limit through either.
const (
	maxSSELine  = 1 << 20
	maxSSEEvent = 1 << 20
)

// sseReader parses Server-Sent Events: "event:" and "data:" fields, events
// separated by a blank line, ":" comment lines ignored, multi-line data
// joined with "\n".
type sseReader struct{ br *bufio.Reader }

func newSSEReader(r io.Reader) *sseReader { return &sseReader{br: bufio.NewReaderSize(r, 32<<10)} }

// next returns the next event. io.EOF means the stream ended cleanly
// between events; io.ErrUnexpectedEOF means it ended mid-event.
func (s *sseReader) next() (event string, data []byte, err error) {
	var buf bytes.Buffer
	seen := false
	for {
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF && seen {
				return event, buf.Bytes(), io.ErrUnexpectedEOF
			}
			return "", nil, err
		}
		if len(line) == 0 {
			if seen {
				return event, buf.Bytes(), nil
			}
			continue // blank lines between events
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			event, seen = string(value), true
		case "data":
			if buf.Len()+len(value)+1 > maxSSEEvent {
				return "", nil, bufio.ErrTooLong
			}
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.Write(value)
			seen = true
		}
	}
}

func (s *sseReader) readLine() ([]byte, error) {
	var line []byte
	for {
		frag, err := s.br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxSSELine {
			return nil, bufio.ErrTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return bytes.TrimRight(line, "\r\n"), nil
			}
			return nil, err
		}
		return bytes.TrimRight(line, "\r\n"), nil
	}
}
