package codex

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderRejectsLineLargerThanTenMiB(t *testing.T) {
	_, err := ReadLine(bufio.NewReader(strings.NewReader(strings.Repeat("x", (10<<20)+1)+"\n")), 10<<20)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestReaderAcceptsSplitCRLFAndExactLimit(t *testing.T) {
	reader := bufio.NewReaderSize(&chunkReader{chunks: [][]byte{
		[]byte("abc"), []byte("def\r"), []byte("\nnext\n"),
	}}, 2)
	line, err := ReadLine(reader, 6)
	if err != nil || string(line) != "abcdef" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	line, err = ReadLine(reader, 6)
	if err != nil || string(line) != "next" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestReaderReturnsFinalCompleteLineWithEOF(t *testing.T) {
	line, err := ReadLine(bufio.NewReader(strings.NewReader(`{"method":"initialized"}`)), 1024)
	if !errors.Is(err, io.EOF) || string(line) != `{"method":"initialized"}` {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

type chunkReader struct {
	chunks [][]byte
}

func (reader *chunkReader) Read(target []byte) (int, error) {
	if len(reader.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := reader.chunks[0]
	reader.chunks = reader.chunks[1:]
	n := copy(target, chunk)
	if n < len(chunk) {
		reader.chunks = append([][]byte{chunk[n:]}, reader.chunks...)
	}
	return n, nil
}
