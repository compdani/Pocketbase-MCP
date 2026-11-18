// Package sse reads the Server-Sent Events wire format used by streaming
// model APIs.
//
// The reader is deliberately small and strict about the parts the adapters
// rely on: LF, CRLF and lone-CR line endings, multi-line data joined with
// "\n", comment lines, fields without a space after the colon, and a final
// event that arrives without a trailing blank line. Parsing is byte-oriented,
// so a frame may be split at any read boundary, including in the middle of a
// multi-byte UTF-8 sequence.
//
// The reader bounds the memory it uses: a line, or the data of one event, that
// grows past the configured maximum fails with ErrTooLarge instead of being
// buffered.
package sse

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// DefaultMaxSize is the default bound on one line and on the data of one
// event: 1 MiB.
const DefaultMaxSize = 1 << 20

// ErrTooLarge reports a line or event that exceeded the reader's size bound.
var ErrTooLarge = errors.New("sse: line or event exceeds the maximum size")

// Event is one dispatched Server-Sent Event. Data has its multi-line payload
// joined with "\n" and no trailing newline. ID is empty when the frame carried
// no id field.
type Event struct {
	Event string
	Data  string
	ID    string
}

// Reader parses a stream of Server-Sent Events.
//
// Next returns io.EOF once the stream is exhausted. After an error other than
// io.EOF the reader is not usable again.
type Reader struct {
	br  *bufio.Reader
	max int

	data    strings.Builder
	event   string
	id      string
	hasData bool
}

// NewReader returns a reader with the default 1 MiB bound.
func NewReader(r io.Reader) *Reader {
	return NewReaderSize(r, DefaultMaxSize)
}

// NewReaderSize returns a reader that fails with ErrTooLarge when a line or an
// event's data exceeds max bytes. A max <= 0 means DefaultMaxSize.
func NewReaderSize(r io.Reader, max int) *Reader {
	if max <= 0 {
		max = DefaultMaxSize
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 4096)
	}
	return &Reader{br: br, max: max}
}

// Next returns the next event, skipping comments and frames that carry no
// data. It returns io.EOF at the end of the stream.
func (r *Reader) Next() (Event, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if r.hasData {
					return r.dispatch(), nil
				}
				return Event{}, io.EOF
			}
			return Event{}, err
		}

		if line == "" {
			if !r.hasData {
				r.reset()
				continue
			}
			return r.dispatch(), nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value := line, ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = strings.TrimPrefix(value, " ")
		}

		switch field {
		case "data":
			add := len(value)
			if r.hasData {
				add++
			}
			if r.data.Len()+add > r.max {
				return Event{}, ErrTooLarge
			}
			if r.hasData {
				r.data.WriteByte('\n')
			}
			r.data.WriteString(value)
			r.hasData = true
		case "event":
			r.event = value
		case "id":
			// The specification disallows NUL in an id; ignore such a line.
			if strings.IndexByte(value, 0) < 0 {
				r.id = value
			}
		default:
			// "retry" and unknown fields are irrelevant to the adapters.
		}
	}
}

// readLine returns one line without its terminator. A final line without a
// newline is returned with a nil error; a subsequent call reports io.EOF.
func (r *Reader) readLine() (string, error) {
	var buf []byte
	for {
		b, err := r.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
		switch b {
		case '\n':
			return string(buf), nil
		case '\r':
			// Consume the LF of a CRLF pair, but leave a lone CR alone so it
			// terminates the next (possibly empty) line.
			if next, err := r.br.Peek(1); err == nil && next[0] == '\n' {
				_, _ = r.br.ReadByte()
			}
			return string(buf), nil
		}
		if len(buf) >= r.max {
			return "", ErrTooLarge
		}
		buf = append(buf, b)
	}
}

func (r *Reader) dispatch() Event {
	ev := Event{Event: r.event, Data: r.data.String(), ID: r.id}
	r.reset()
	return ev
}

func (r *Reader) reset() {
	r.data.Reset()
	r.event = ""
	r.id = ""
	r.hasData = false
}
