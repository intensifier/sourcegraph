package google

import (
	"bufio"
	"bytes"
	"io"

	"github.com/sourcegraph/sourcegraph/lib/errors"
)

const maxPayloadSize = 10 * 1024 * 1024 // 10mb

var doneBytes = []byte("[DONE]")

// decoder decodes streaming events from a Server Sent Event stream.
type decoder struct {
	scanner *bufio.Scanner
	done    bool
	data    []byte
	err     error
}

func NewDecoder(r io.Reader) *decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), maxPayloadSize)
	// bufio.ScanLines, except we look for \r\n\r\n which separate events.
	split := func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
			return i + 4, data[:i], nil
		}
		if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
			return i + 2, data[:i], nil
		}
		// If we're at EOF, we have a final, non-terminated event. This should
		// be empty.
		if atEOF {
			return len(data), data, nil
		}
		// Request more data.
		return 0, nil, nil
	}
	scanner.Split(split)
	return &decoder{
		scanner: scanner,
	}
}

// Scan advances the decoder to the next event in the stream. It returns
// false when it either hits the end of the stream or an error.
func (d *decoder) Scan() bool {
	if d.done {
		return false
	}
	for d.scanner.Scan() {
		line := d.scanner.Bytes()
		typ, data := splitColon(line)
		switch {
		case bytes.Equal(typ, []byte("data")):
			d.data = data
			// Check for special sentinel value used by the Google API to
			// indicate that the stream is done.
			if bytes.Equal(data, doneBytes) {
				d.done = true
				return false
			}
			return true
		default:
			d.err = errors.Errorf("malformed data, expected data: %s", typ)
			return false
		}
	}

	d.err = d.scanner.Err()
	return false
}

// Event returns the event data of the last decoded event
func (d *decoder) Data() []byte {
	return d.data
}

// Err returns the last encountered error
func (d *decoder) Err() error {
	return d.err
}

func splitColon(data []byte) ([]byte, []byte) {
	i := bytes.Index(data, []byte(":"))
	if i < 0 {
		return bytes.TrimSpace(data), nil
	}
	return bytes.TrimSpace(data[:i]), bytes.TrimSpace(data[i+1:])
}
