package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
)

const streamBody = `{"model":"mock/small","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`

// readSSE returns every data payload until [DONE] or EOF.
func readSSE(t *testing.T, resp *http.Response) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, d)
			if d == "[DONE]" {
				break
			}
		}
	}
	return out
}

func TestStreamEndToEnd(t *testing.T) {
	g := newGateway(t, gwOpts{primary: mockupstream.Config{StreamChunks: 5}})
	resp := g.post(t, streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readSSE(t, resp)
	if len(events) == 0 || events[len(events)-1] != "[DONE]" {
		t.Fatalf("missing [DONE]: %v", events)
	}
	var text strings.Builder
	var usage *api.Usage
	ids := map[string]bool{}
	for _, e := range events[:len(events)-1] {
		var c api.ChatChunk
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", e, err)
		}
		ids[c.ID] = true
		if c.Model != "mock/small" || c.Object != "chat.completion.chunk" {
			t.Errorf("chunk fields: %+v", c)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if text.String() != "tok0 tok1 tok2 tok3 tok4 " || usage == nil || usage.CompletionTokens != 5 || len(ids) != 1 {
		t.Fatalf("text=%q usage=%+v ids=%v", text.String(), usage, ids)
	}
	o := g.last()
	if o.Status != 200 || !o.Stream || o.TTFT <= 0 || o.TokensOut != 5 || o.UsageEstimated || o.ErrorClass != "" || o.Cache != "BYPASS" {
		t.Errorf("outcome %+v", o)
	}
}

func TestStreamWithoutUsageRequestOmitsUsageChunk(t *testing.T) {
	g := newGateway(t, gwOpts{})
	for _, e := range readSSE(t, g.post(t, `{"model":"mock/small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)) {
		if strings.Contains(e, `"usage"`) {
			t.Fatalf("usage sent without stream_options.include_usage: %s", e)
		}
	}
	if o := g.last(); o.TokensOut != 10 { // still recorded for the ledger
		t.Errorf("outcome %+v", o)
	}
}

func TestStreamFallbackBeforeFirstChunk(t *testing.T) {
	g := newGateway(t, gwOpts{primary: mockupstream.Config{ErrorRate: 1}, withBackup: true})
	resp := g.post(t, streamBody)
	if resp.StatusCode != 200 || resp.Header.Get("X-Tollgate-Provider") != "backup" {
		t.Fatalf("status %d provider %q", resp.StatusCode, resp.Header.Get("X-Tollgate-Provider"))
	}
	if ev := readSSE(t, resp); ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("events %v", ev)
	}
}

func TestStreamInterrupted(t *testing.T) {
	g := newGateway(t, gwOpts{primary: mockupstream.Config{StreamChunks: 10, StreamFailAfter: 3}, withBackup: true})
	events := readSSE(t, g.post(t, streamBody))
	if len(events) < 2 || events[len(events)-1] != "[DONE]" || !strings.Contains(events[len(events)-2], `"stream_interrupted"`) {
		t.Fatalf("want error event then [DONE], got %v", events)
	}
	o := g.last()
	if o.ErrorClass != "stream_interrupted" || o.Provider != "mock" || !o.UsageEstimated || o.TokensOut == 0 {
		t.Errorf("no fallback after the first chunk; outcome %+v", o)
	}
	if g.backup.Requests() != 0 {
		t.Error("fallback must not happen mid-stream")
	}
}

func TestStreamClientDisconnectCancelsUpstream(t *testing.T) {
	g := newGateway(t, gwOpts{primary: mockupstream.Config{StreamChunks: 200, ChunkDelay: 10 * time.Millisecond}, timeout: 10 * time.Second})
	resp := g.post(t, streamBody)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && !strings.Contains(sc.Text(), "tok0") {
	}
	_ = resp.Body.Close() // client goes away mid-stream
	for range 200 {
		if g.primary.Canceled() == 1 {
			if o := g.last(); o.ErrorClass != "client_canceled" {
				t.Errorf("outcome %+v", o)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upstream kept generating after the client disconnected")
}
