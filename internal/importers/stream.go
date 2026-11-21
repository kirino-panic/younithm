package importers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// eachJSONLine calls fn for each non-blank line, with no line-length limit.
// idx is the 1-based physical line number.
func eachJSONLine(r io.Reader, fn func(idx int, line []byte) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	idx := 0
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			idx++
			t := bytes.TrimSpace(line)
			if len(t) > 0 {
				if ferr := fn(idx, t); ferr != nil {
					return ferr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// eachArrayElem streams the elements of a JSON array (the '[' token is read
// here). Elements are handed over raw so a wrongly-typed element is a row
// error, not a fatal one.
func eachArrayElem(dec *json.Decoder, fn func(idx int, raw json.RawMessage) error) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading array start: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("expected a JSON array")
	}
	idx := 0
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("element %d: %w", idx+1, err)
		}
		idx++
		if err := fn(idx, raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // ']'
		return fmt.Errorf("reading array end: %w", err)
	}
	return nil
}

// eachKeyedArray walks the top-level object and streams the array under key,
// skipping every other value. A missing key is an error.
func eachKeyedArray(dec *json.Decoder, key string, fn func(idx int, raw json.RawMessage) error) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading object start: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("expected a JSON object")
	}
	found := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		k, _ := kt.(string)
		if k == key && !found {
			found = true
			if err := eachArrayElem(dec, fn); err != nil {
				return err
			}
			continue
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("no %q array in file", key)
	}
	return nil
}

// peekFirstByte returns a reader that still yields everything, plus the first
// non-space byte (a UTF-8 BOM is skipped).
func peekFirstByte(r io.Reader) (io.Reader, byte, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			return br, 0, err
		}
		if b == ' ' || b == '\n' || b == '\r' || b == '\t' {
			continue
		}
		_ = br.UnreadByte()
		return br, b, nil
	}
}
