package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Retrying is only ever done on a request whose response has not been handed to
// a caller yet. That is why the streaming reads are not covered: re-running one
// would replay tokens the user has already seen. Everything below is safe to
// repeat — a metadata call, or the request phase of a stream that failed before
// its body was returned.
const (
	maxAttempts   = 3
	retryBaseWait = 250 * time.Millisecond
	retryMaxWait  = 2 * time.Second
)

// retryable reports whether err is worth another attempt.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller's decision, not a hiccup.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	// A transport failure — a backend still starting up refusing connections, a
	// reset connection — is usually transient.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// sleepBackoff waits before the next attempt, doubling each time. It returns
// false when the context ended first, so a Stop does not have to wait it out.
func sleepBackoff(ctx context.Context, attemptNo int) bool {
	wait := retryBaseWait << attemptNo
	if wait > retryMaxWait {
		wait = retryMaxWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// withRetry runs attempt until it succeeds, fails unrecoverably, or runs out of
// attempts. The last real error is reported rather than the cancellation that
// ended a wait, because "the backend returned 503 twice" is the actionable fact.
func withRetry[T any](ctx context.Context, attempt func() (T, error)) (T, error) {
	var (
		lastErr error
		zero    T
	)
	for i := 0; i < maxAttempts; i++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, ctxErr
		}
		value, err := attempt()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable(err) || i == maxAttempts-1 {
			break
		}
		if !sleepBackoff(ctx, i) {
			break
		}
	}
	return zero, lastErr
}

// credentialHeaders are the request headers whose value must never survive into
// an error message. A gateway or reverse proxy occasionally echoes the request
// back in its error page, and HTTPError.Body carries that text verbatim to a
// toast or a log.
var credentialHeaders = map[string]bool{
	"authorization":        true,
	"x-subscription-token": true,
	"api-key":              true,
	"x-api-key":            true,
	"x-goog-api-key":       true,
}

// redactHeaders removes every credential we sent from an error body, so a
// provider cannot leak its own key by repeating what it was told.
func redactHeaders(err error, headers map[string]string) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Body == "" {
		return err
	}
	for name, value := range headers {
		if !credentialHeaders[strings.ToLower(name)] {
			continue
		}
		for _, secret := range []string{value, strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))} {
			// A short value is a generic word, not a key; redacting it would
			// mangle the message without protecting anything.
			if len(secret) < 8 {
				continue
			}
			httpErr.Body = strings.ReplaceAll(httpErr.Body, secret, "[redacted]")
		}
	}
	return err
}

// doJSON performs a request, decodes the JSON body into out, and converts
// non-2xx responses into *HTTPError so callers can react to status codes. A
// transient failure is retried before it is reported.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, doJSONOnce(ctx, client, method, url, headers, body, out)
	})
	return err
}

func doJSONOnce(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
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
		return redactHeaders(&HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(raw)),
			URL:        url,
		}, headers)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postStream issues a streaming POST and hands back the raw response body. The
// caller owns closing it.
//
// Only the request phase is retried, and it completes before the caller starts
// reading, so a retry cannot duplicate output. A response lost after the backend
// had already begun generating would cost one wasted generation, which is the
// price of not failing the turn outright.
func postStream(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
	return withRetry(ctx, func() (io.ReadCloser, error) {
		return postStreamOnce(ctx, url, headers, body)
	})
}

func postStreamOnce(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
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
		return nil, redactHeaders(&HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(raw)),
			URL:        url,
		}, headers)
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
