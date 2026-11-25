package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// anthropicVersion pins the Messages API version (anthropic-version header).
// On 2026-10-21 the API rejects temperature/top_p/top_k on every model, so
// this adapter does not forward sampling parameters (see translate).
const anthropicVersion = "2026-10-21"

// Anthropic adapts the OpenAI schema to Anthropic's Messages API.
type Anthropic struct {
	baseURL          string
	hdr              http.Header
	client           *http.Client
	defaultMaxTokens int
}

func NewAnthropic(baseURL, apiKey string, defaultMaxTokens int, client *http.Client) *Anthropic {
	hdr := http.Header{}
	hdr.Set("x-api-key", apiKey)
	hdr.Set("anthropic-version", anthropicVersion)
	return &Anthropic{baseURL: strings.TrimRight(baseURL, "/"), hdr: hdr, client: client, defaultMaxTokens: defaultMaxTokens}
}

func (p *Anthropic) Name() string { return "anthropic" }

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"` // required by the Messages API
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicResponse struct {
	ID         string           `json:"id"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// translate maps an OpenAI request to the Messages API:
//   - system messages move to the top-level "system" field;
//   - content becomes text blocks; non-text parts are rejected (400);
//   - stop (string or array) becomes stop_sequences;
//   - max_tokens is required, so a default fills it in;
//   - temperature/top_p are dropped: API version 2026-10-21 rejects them and
//     current models sample only at their defaults (documented limitation);
//   - tools / tool messages are not translated in the MVP (400).
func (p *Anthropic) translate(req *api.ChatRequest) (*anthropicRequest, *ProviderError) {
	if len(req.Tools) > 0 {
		return nil, badRequest(p.Name(), "tool calling is not supported")
	}
	out := &anthropicRequest{Model: req.Model, MaxTokens: p.defaultMaxTokens, Stream: req.Stream}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}
	var system []string
	for _, m := range req.Messages {
		text, err := m.Text()
		if err != nil {
			return nil, badRequest(p.Name(), err.Error())
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user", "assistant":
			out.Messages = append(out.Messages, anthropicMessage{Role: m.Role, Content: []anthropicBlock{{Type: "text", Text: text}}})
		default:
			return nil, badRequest(p.Name(), "message role "+m.Role+" is not supported")
		}
	}
	if len(out.Messages) == 0 {
		return nil, badRequest(p.Name(), "at least one user message is required")
	}
	out.System = strings.Join(system, "\n\n")
	if len(req.Stop) > 0 {
		var one string
		if json.Unmarshal(req.Stop, &one) == nil {
			out.StopSequences = []string{one}
		} else if json.Unmarshal(req.Stop, &out.StopSequences) != nil {
			return nil, badRequest(p.Name(), "stop must be a string or an array of strings")
		}
	}
	return out, nil
}

// finishReason maps Anthropic stop reasons to OpenAI finish reasons.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

func (p *Anthropic) Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error) {
	areq, perr := p.translate(req)
	if perr != nil {
		return nil, perr
	}
	areq.Stream = false
	resp, err := post(ctx, p.client, p.Name(), p.baseURL+"/v1/messages", p.hdr, areq)
	if err != nil {
		return nil, err
	}
	var ar anthropicResponse
	if err := decode(ctx, p.Name(), resp, &ar); err != nil {
		return nil, err
	}
	// Only text blocks carry the answer; thinking blocks (empty by default
	// on current models) and others are skipped.
	var text strings.Builder
	for _, b := range ar.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return &api.ChatResponse{
		ID:      ar.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []api.Choice{{
			Message:      api.Message{Role: "assistant", Content: api.StringContent(text.String())},
			FinishReason: finishReason(ar.StopReason),
		}},
		Usage: &api.Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}, nil
}
