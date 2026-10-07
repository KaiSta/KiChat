package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func countingServer(t *testing.T, attempts *int32, fail func(n int32, w http.ResponseWriter) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(attempts, 1)
		if fail(n, w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONRetriesTransientFailures(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(n int32, w http.ResponseWriter) bool {
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"model is loading"}`))
			return true
		}
		return false
	})

	var out struct {
		OK bool `json:"ok"`
	}
	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, &out)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Error("the retried response body was not decoded into out")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestDoJSONDoesNotRetryAClientError(t *testing.T) {
	var attempts int32
	srv := countingServer(t, &attempts, func(_ int32, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
		return true
	})

	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, nil)
	if err == nil {
		t.Fatal("want an error from a 400 response")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Errorf("error = %v, want an *HTTPError with status 400", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: a 400 must not be retried", got)
	}
}

// TestDoJSONStopsWhenContextEnds pins that a Stop does not have to wait out the
// backoff: once the context is done, no further attempt is made.
func TestDoJSONStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		cancel() // ends the wait between attempts
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	if err := doJSON(ctx, &http.Client{Timeout: 5 * time.Second},
		http.MethodGet, srv.URL, nil, nil, nil); err == nil {
		t.Fatal("want an error once the context ended")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: the context ended before the retry", got)
	}
}

func TestPostStreamRetriesTheRequestPhase(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	t.Cleanup(srv.Close)

	rc, err := postStream(context.Background(), srv.URL, nil, map[string]any{"stream": true})
	if err != nil {
		t.Fatalf("postStream: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		t.Error("the retried stream returned an empty body")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestHTTPErrorRedactsOurOwnCredential covers a gateway that echoes the request
// back in its error page: the key must not survive into the message the user sees.
func TestHTTPErrorRedactsOurOwnCredential(t *testing.T) {
	const key = "sk-not-a-real-key-1234567890"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// A proxy-style error page that repeats what it was sent.
		_, _ = w.Write([]byte(`{"error":"bad request","echoed":"` +
			r.Header.Get("Authorization") + `"}`))
	}))
	t.Cleanup(srv.Close)

	err := doJSON(context.Background(), &http.Client{Timeout: 5 * time.Second},
		http.MethodPost, srv.URL, map[string]string{"Authorization": "Bearer " + key}, nil, nil)
	if err == nil {
		t.Fatal("want an error from a 400 response")
	}
	message := err.Error()
	if strings.Contains(message, key) || strings.Contains(message, "Bearer "+key) {
		t.Errorf("the API key leaked into the error: %s", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Errorf("expected a redaction marker in the error: %s", message)
	}
}

// A header that is not a credential must be left alone, so a body that happens
// to quote a content type is not mangled.
func TestRedactionLeavesNonCredentialHeadersAlone(t *testing.T) {
	err := &HTTPError{StatusCode: 400, Body: `sent as application/json`}
	redactHeaders(err, map[string]string{"Content-Type": "application/json"})
	if err.Body != `sent as application/json` {
		t.Errorf("body was rewritten: %s", err.Body)
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"throttled", &HTTPError{StatusCode: http.StatusTooManyRequests}, true},
		{"server error", &HTTPError{StatusCode: http.StatusInternalServerError}, true},
		{"unavailable", &HTTPError{StatusCode: http.StatusServiceUnavailable}, true},
		{"bad request", &HTTPError{StatusCode: http.StatusBadRequest}, false},
		{"not found", &HTTPError{StatusCode: http.StatusNotFound}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped cancellation", context.DeadlineExceeded, false},
		{"transport failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
	}
	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
