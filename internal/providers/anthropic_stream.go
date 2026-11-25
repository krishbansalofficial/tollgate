package providers

import (
	"context"
	"encoding/json"
	"io"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// anthropicEvent covers the fields used across Messages API stream events.
type anthropicEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ChatStream translates the Messages API event stream to OpenAI chunks:
//
//	message_start        -> role chunk (and remembers input tokens)
//	content_block_delta  -> content chunk for text_delta; other delta types
//	                        (thinking, signatures, tool JSON) are skipped
//	message_delta        -> final chunk with finish_reason and usage
//	message_stop         -> end of stream
//	error                -> ProviderError
//	ping, content_block_start/stop -> nothing
func (p *Anthropic) ChatStream(ctx context.Context, req *api.ChatRequest) (Stream, error) {
	areq, perr := p.translate(req)
	if perr != nil {
		return nil, perr
	}
	areq.Stream = true
	resp, err := post(ctx, p.client, p.Name(), p.baseURL+"/v1/messages", sseHeaders(p.hdr), areq)
	if err != nil {
		return nil, err
	}
	inputTokens := 0 // only touched by this stream's single reader goroutine
	return newSSEStream(ctx, p.Name(), resp, func(_ string, data []byte) (*api.ChatChunk, error) {
		var ev anthropicEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, &ProviderError{Provider: p.Name(), Retryable: true, Message: "decode event: " + err.Error()}
		}
		switch ev.Type {
		case "message_start":
			inputTokens = ev.Message.Usage.InputTokens
			return api.TextChunk("assistant", "", ""), nil
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				return api.TextChunk("", ev.Delta.Text, ""), nil
			}
		case "message_delta":
			c := api.TextChunk("", "", finishReason(ev.Delta.StopReason))
			c.Usage = &api.Usage{PromptTokens: inputTokens, CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens: inputTokens + ev.Usage.OutputTokens}
			return c, nil
		case "message_stop":
			return nil, io.EOF
		case "error":
			return nil, streamError(p.Name(), data)
		}
		return nil, nil
	}), nil
}
