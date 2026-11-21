package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcMessage struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func readCED(r *bufio.Reader, maxHeader, maxBody int) ([]byte, error) {
	const prefix = "Content-Length: "
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line)+2 > maxHeader || !bytes.HasPrefix(line, []byte(prefix)) || !bytes.HasSuffix(line, []byte("\r\n")) {
		return nil, errors.New("invalid CED header")
	}
	value := string(line[len(prefix) : len(line)-2])
	if strings.Trim(value, "0123456789") != "" {
		return nil, errors.New("invalid CED length")
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > maxBody {
		return nil, errors.New("invalid CED length")
	}
	var separator [2]byte
	if _, err := io.ReadFull(r, separator[:]); err != nil || string(separator[:]) != "\r\n" {
		return nil, errors.New("invalid CED header separator")
	}
	body := make([]byte, n)
	_, err = io.ReadFull(r, body)
	return body, err
}

func writeCED(w io.Writer, message any, maxHeader, maxBody int) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if len(body) > maxBody || len(header) > maxHeader {
		return errors.New("CED response exceeds negotiated limits")
	}
	for _, part := range [][]byte{[]byte(header), body} {
		for len(part) != 0 {
			n, err := w.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}

func validateJSON(data []byte, maxDepth int, allowNULValues bool) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	tokens := 0
	read := func() (json.Token, error) {
		tokens++
		if tokens > 32768 {
			return nil, errors.New("JSON token limit exceeded")
		}
		return decoder.Token()
	}
	var value func(json.Token, int) error
	value = func(token json.Token, depth int) error {
		switch item := token.(type) {
		case string:
			if !allowNULValues && strings.ContainsRune(item, 0) {
				return errors.New("NUL is forbidden")
			}
		case json.Number:
			n, err := item.Float64()
			if err != nil || math.IsInf(n, 0) {
				return errors.New("invalid JSON number")
			}
		case json.Delim:
			if depth > maxDepth {
				return errors.New("JSON depth limit exceeded")
			}
			keys := map[string]bool{}
			for decoder.More() {
				next, err := read()
				if err != nil {
					return err
				}
				if item == '{' {
					key, ok := next.(string)
					if !ok || keys[key] || strings.ContainsRune(key, 0) {
						return errors.New("invalid or duplicate JSON key")
					}
					keys[key] = true
					next, err = read()
					if err != nil {
						return err
					}
				}
				if err := value(next, depth+1); err != nil {
					return err
				}
			}
			_, err := read()
			return err
		}
		return nil
	}
	first, err := read()
	if err != nil {
		return err
	}
	if err := value(first, 1); err != nil {
		return err
	}
	if _, err := read(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
