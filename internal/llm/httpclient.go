package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// jsonClient is used for the short metadata calls (model listings, health).
var jsonClient = &http.Client{Timeout: 15 * time.Second}

// streamClient has no total timeout: generations can legitimately run for
// minutes. Cancellation is driven entirely by the request context.
var streamClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	},
}

// doJSON performs a request, decodes the JSON body into out, and converts
// non-2xx responses into *HTTPError so callers can react to status codes.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(raw)),
			URL:        url,
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postStream issues a streaming POST and hands back the raw response body. The
// caller owns closing it.
func postStream(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(raw)),
			URL:        url,
		}
	}
	return resp.Body, nil
}

// sseReader parses a text/event-stream body. It is deliberately tolerant of
// payloads split at arbitrary byte boundaries, which is the failure mode that
// breaks naive streaming clients.
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the data payload of the next event. It returns io.EOF once the
// stream is exhausted.
func (s *sseReader) Next() ([]byte, error) {
	var data [][]byte
	for {
		line, err := s.br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(string(line), "\r\n")
			switch {
			case trimmed == "":
				// Blank line terminates an event.
				if len(data) > 0 {
					return bytes.Join(data, []byte("\n")), nil
				}
			case strings.HasPrefix(trimmed, ":"):
				// Comment / keep-alive.
			case strings.HasPrefix(trimmed, "data:"):
				payload := strings.TrimPrefix(trimmed, "data:")
				payload = strings.TrimPrefix(payload, " ")
				data = append(data, []byte(payload))
			default:
				// event:, id:, retry: — not needed by these backends.
			}
		}
		if err != nil {
			if err == io.EOF && len(data) > 0 {
				return bytes.Join(data, []byte("\n")), nil
			}
			return nil, err
		}
	}
}

// ndjsonReader decodes newline-delimited JSON, tolerating any chunking because
// json.Decoder buffers internally.
type ndjsonReader struct {
	dec *json.Decoder
}

func newNDJSONReader(r io.Reader) *ndjsonReader {
	return &ndjsonReader{dec: json.NewDecoder(bufio.NewReaderSize(r, 64*1024))}
}

// Next decodes the next JSON value into out. Returns io.EOF at the end.
func (n *ndjsonReader) Next(out any) error {
	return n.dec.Decode(out)
}
