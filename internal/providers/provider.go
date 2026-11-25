// Package providers defines the Provider interface and the adapters that
// translate Tollgate's canonical (OpenAI) schema to each upstream's API.
package providers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// Provider is one upstream LLM API. req.Model is already the provider-native
// model ID; the router strips the "provider/" prefix before calling.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error)
	// ChatStream opens a streamed completion. Errors before the stream opens
	// are returned here; errors after it opens come from Stream.Recv.
	ChatStream(ctx context.Context, req *api.ChatRequest) (Stream, error)
}

// Stream yields OpenAI-shaped chunks. Recv returns io.EOF after the last
// chunk; any other error means the stream broke. Close must always be called
// and releases the upstream connection. Chunks may carry Usage (typically the
// last one); callers must not assume a usage chunk arrives.
type Stream interface {
	Recv() (*api.ChatChunk, error)
	Close() error
}

// ProviderError is every upstream failure, normalised so the router can
// decide retry/fallback without knowing which provider it came from.
type ProviderError struct {
	Provider   string
	Status     int           // upstream HTTP status; 0 for transport errors
	Retryable  bool          // 408, 429, 5xx, timeouts, connection failures
	RetryAfter time.Duration // from the upstream Retry-After header, if any
	Message    string        // for logs; may be a raw upstream body
	// FromEnvelope is true when Message came from a parsed {"error":{"message"}}
	// envelope. Only then is it safe to relay to clients: a raw body could be
	// an HTML error page or a proxy message containing internal details.
	FromEnvelope bool
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: upstream status %d: %s", e.Provider, e.Status, e.Message)
}

// credentialHints mark upstream messages about the gateway's own account
// rather than the client's request. They are never relayed to clients.
var credentialHints = []string{"api key", "api_key", "apikey", "credential", "billing", "quota", "project", "organization"}

// ClientMessage returns the upstream message if it is safe to show the
// client: it came from a parsed error envelope and does not mention the
// gateway's credentials, account or quota.
func (e *ProviderError) ClientMessage() (string, bool) {
	if !e.FromEnvelope {
		return "", false
	}
	lower := strings.ToLower(e.Message)
	for _, h := range credentialHints {
		if strings.Contains(lower, h) {
			return "", false
		}
	}
	return e.Message, true
}

// ClientFault reports an error caused by the client's own request. Another
// provider would reject it too, so the router neither retries nor falls back,
// and the message may be shown to the client. 401/403/402 are deliberately
// excluded: they describe the gateway's upstream credentials or billing.
func (e *ProviderError) ClientFault() bool {
	switch e.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// retryableStatus reports whether retrying the same request could succeed.
// 4xx other than 408/429 mean the request itself is bad, so a retry (or a
// different provider) would fail the same way.
func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// parseRetryAfter handles both forms allowed by RFC 9110: delay-seconds and
// an HTTP-date. Unparseable or past values yield 0.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// NewHTTPClient returns the client shared by all requests to one provider.
// http.DefaultTransport keeps only 2 idle connections per host, so under
// concurrency most requests would pay a fresh TCP+TLS handshake to the same
// upstream. Raising MaxIdleConnsPerHost keeps connections warm.
//
// No client-level Timeout: deadlines come from the request context, so one
// mechanism controls both cancellation and timeouts (and streaming, which
// can legitimately run for minutes, isn't killed by a fixed timeout).
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 0 // governed by ctx; LLMs can take long before the first header
	return &http.Client{Transport: t}
}
