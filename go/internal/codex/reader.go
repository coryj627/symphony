package codex

import (
	"bufio"
	"errors"
	"io"
)

// ReadLine reads one bounded physical JSONL line and removes its line ending.
// A final unterminated line is returned together with io.EOF for the caller to
// decode before treating the transport as closed.
func ReadLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	line := make([]byte, 0, min(maxBytes, 4096))
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > maxBytes+2 {
			return nil, ErrMessageTooLarge
		}
		switch {
		case err == nil:
			line = trimLineEnding(line)
			if len(line) > maxBytes {
				return nil, ErrMessageTooLarge
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			line = trimLineEnding(line)
			if len(line) > maxBytes {
				return nil, ErrMessageTooLarge
			}
			return line, io.EOF
		default:
			return nil, err
		}
	}
}

func trimLineEnding(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}
