package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

const (
	// ponytail: fixed cap on a stream's total duration; make it configurable
	// if real workloads need longer generations.
	maxStreamDuration = 10 * time.Minute
	// A client that stops reading for this long is treated as gone, so a
	// stalled reader can't pin a goroutine and an upstream connection.
	streamWriteTimeout = 10 * time.Second
)

// serveStream proxies a streamed completion as OpenAI-style SSE.
//
// Retry and fallback happen inside Router.ChatStream, before the first chunk.
// Once the headers are written the response is committed: an upstream
// failure after that is reported in-band (an error event, then [DONE]) and
// recorded as stream_interrupted. The client hanging up cancels r.Context(),
// which cancels the upstream request, so the provider stops generating.
func (s *Server) serveStream(q *request, bg context.Context) {
	r, w := q.r, q.w
	ctx, cancel := context.WithTimeout(r.Context(), maxStreamDuration)
	defer cancel()

	res, err := s.Router.ChatStream(ctx, &q.body)
	if err != nil {
		s.reconcile(bg, q, 0)
		s.upstreamError(q, err)
		return
	}
	defer func() { _ = res.Stream.Close() }()
	q.out.Model, q.out.Provider, q.out.Attempts = res.Model, res.Provider, res.Attempts
	upstream := res.Upstream // grows below with time spent waiting on Recv

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to buffer the stream
	h.Set("X-Tollgate-Cache", "BYPASS")
	h.Set("X-Tollgate-Provider", res.Provider)
	w.WriteHeader(http.StatusOK)
	q.out.Status = http.StatusOK

	rc := http.NewResponseController(w)
	send := func(payload []byte) error {
		// Per-write deadline instead of a server-wide WriteTimeout, which
		// would cap the whole response and kill long streams.
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if _, err := w.Write(payload); err != nil {
			return err
		}
		return rc.Flush() // without a flush, chunks sit in the buffer and TTFT is meaningless
	}
	sendData := func(v any) error {
		b, _ := json.Marshal(v)
		return send(append(append([]byte("data: "), b...), '\n', '\n'))
	}

	id, created := "chatcmpl-"+newID(), time.Now().Unix()
	var usage *api.Usage
	streamed := 0 // content bytes, for estimating usage if the upstream sends none
	clean := false
	for {
		t0 := time.Now()
		c, err := res.Stream.Recv()
		upstream += time.Since(t0)
		if errors.Is(err, io.EOF) {
			clean = true
			break
		}
		if err != nil {
			if r.Context().Err() != nil {
				q.out.ErrorClass = "client_canceled"
			} else {
				q.out.ErrorClass = "stream_interrupted"
				s.Logger.WarnContext(r.Context(), "stream interrupted", "request_id", q.out.RequestID, "provider", res.Provider, "err", err)
				_ = sendData(api.ErrorResponse{Error: api.ErrorBody{Message: "upstream stream interrupted", Type: "api_error", Code: "stream_interrupted"}})
				_ = send([]byte("data: [DONE]\n\n"))
			}
			break
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if len(c.Choices) == 0 {
			continue // usage-only chunk: re-emitted at the end only if the client asked
		}
		// One ID/model per response regardless of provider; usage is
		// stripped here and sent (if requested) in its own final chunk, as
		// OpenAI does.
		c.ID, c.Created, c.Model, c.Object, c.Usage = id, created, res.Model, "chat.completion.chunk", nil
		for _, ch := range c.Choices {
			streamed += len(ch.Delta.Content)
		}
		if err := sendData(c); err != nil {
			q.out.ErrorClass = "client_canceled" // client stopped reading
			break
		}
		if q.out.TTFT == 0 && c.HasContent() {
			q.out.TTFT = time.Since(q.start)
		}
	}
	if clean {
		if usage != nil && q.body.StreamOptions != nil && q.body.StreamOptions.IncludeUsage {
			_ = sendData(api.ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: res.Model, Choices: []api.ChunkChoice{}, Usage: usage})
		}
		_ = send([]byte("data: [DONE]\n\n"))
	}
	q.out.Upstream = upstream

	if usage == nil {
		usage = &api.Usage{PromptTokens: api.EstimatePromptTokens(&q.body), CompletionTokens: streamed / 4}
		q.out.UsageEstimated = true
	}
	used := s.recordUsage(q, usage, res.Model)
	s.reconcile(bg, q, used)
}
