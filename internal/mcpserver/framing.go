package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func readMessage(r *bufio.Reader) ([]byte, bool, error) {
	firstLine, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF && firstLine == "" {
			return nil, false, io.EOF
		}
		return nil, false, err
	}

	if payload, ok, err := tryReadJSONLineMessage(r, firstLine); ok || err != nil {
		return payload, ok, err
	}

	contentLength := -1
	sawHeader := false
	line := firstLine

	for {
		if line == "\r\n" {
			if !sawHeader {
				if line, err = r.ReadString('\n'); err != nil {
					if err == io.EOF && !sawHeader {
						return nil, false, io.EOF
					}
					return nil, false, err
				}
				continue
			}
			break
		}

		sawHeader = true
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}

		if strings.EqualFold(key, "Content-Length") {
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				return nil, false, fmt.Errorf("invalid Content-Length: %w", parseErr)
			}
			contentLength = parsed
		}

		line, err = r.ReadString('\n')
		if err != nil {
			if err == io.EOF && !sawHeader {
				return nil, false, io.EOF
			}
			return nil, false, err
		}
	}

	if contentLength < 0 {
		return nil, false, fmt.Errorf("missing Content-Length header")
	}

	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, err
	}

	return payload, false, nil
}

func tryReadJSONLineMessage(r *bufio.Reader, firstLine string) ([]byte, bool, error) {
	trimmed := strings.TrimSpace(firstLine)
	if trimmed == "" {
		return nil, false, nil
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return nil, false, nil
	}

	buf := bytes.NewBufferString(firstLine)
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		return bytes.TrimSpace(buf.Bytes()), true, nil
	}

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, true, err
		}
		buf.WriteString(line)
		if json.Valid(bytes.TrimSpace(buf.Bytes())) {
			return bytes.TrimSpace(buf.Bytes()), true, nil
		}
	}
}

func writeFramedMessage(w *bufio.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}

func writeJSONLineMessage(w *bufio.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}
