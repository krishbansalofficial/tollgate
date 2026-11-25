package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
)

// Gemini adapts the OpenAI schema to Google's native generateContent API.
// The key goes in the x-goog-api-key header, never the URL, so it can't leak
// into access logs or traces that record URLs.
type Gemini struct {
	baseURL string // e.g. https://generativelanguage.googleapis.com/v1beta
	hdr     http.Header
	client  *http.Client
}

func NewGemini(baseURL, apiKey string, client *http.Client) *Gemini {
	hdr := http.Header{}
	hdr.Set("x-goog-api-key", apiKey)
	return &Gemini{baseURL: strings.TrimRight(baseURL, "/"), hdr: hdr, client: client}
}

func (p *Gemini) Name() string { return "gemini" }

type geminiPart struct {
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"` // reasoning summary parts on thinking models: not part of the answer
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGeneration `json:"generationConfig"`
}

type geminiGeneration struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
}

type geminiResponse struct {
	ResponseID string `json:"responseId"`
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

// usage counts thinking tokens as completion tokens: Gemini bills them as
// output, so leaving them out would under-report cost.
func (r *geminiResponse) usage() *api.Usage {
	if r.UsageMetadata == nil {
		return nil
	}
	u := r.UsageMetadata
	out := u.CandidatesTokenCount + u.ThoughtsTokenCount
	return &api.Usage{PromptTokens: u.PromptTokenCount, CompletionTokens: out, TotalTokens: u.PromptTokenCount + out}
}

// text joins answer parts of the first candidate, skipping thought parts.
func (r *geminiResponse) text() (string, string) {
	if len(r.Candidates) == 0 {
		return "", ""
	}
	var b strings.Builder
	for _, part := range r.Candidates[0].Content.Parts {
		if !part.Thought {
			b.WriteString(part.Text)
		}
	}
	return b.String(), r.Candidates[0].FinishReason
}

func geminiFinish(reason string) string {
	switch reason {
	case "":
		return ""
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	default: // STOP, OTHER, ...
		return "stop"
	}
}

// translate: system messages become systemInstruction; assistant becomes
// role "model"; stop becomes stopSequences. Tools and non-text parts are 400s.
func (p *Gemini) translate(req *api.ChatRequest) (*geminiRequest, *ProviderError) {
	if len(req.Tools) > 0 {
		return nil, badRequest(p.Name(), "tool calling is not supported")
	}
	out := &geminiRequest{GenerationConfig: geminiGeneration{MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP}}
	var system []geminiPart
	for _, m := range req.Messages {
		text, err := m.Text()
		if err != nil {
			return nil, badRequest(p.Name(), err.Error())
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, geminiPart{Text: text})
		case "user":
			out.Contents = append(out.Contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: text}}})
		case "assistant":
			out.Contents = append(out.Contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: text}}})
		default:
			return nil, badRequest(p.Name(), "message role "+m.Role+" is not supported")
		}
	}
	if len(out.Contents) == 0 {
		return nil, badRequest(p.Name(), "at least one user message is required")
	}
	if len(system) > 0 {
		out.SystemInstruction = &geminiContent{Parts: system}
	}
	if len(req.Stop) > 0 {
		var one string
		if json.Unmarshal(req.Stop, &one) == nil {
			out.GenerationConfig.StopSequences = []string{one}
		} else if json.Unmarshal(req.Stop, &out.GenerationConfig.StopSequences) != nil {
			return nil, badRequest(p.Name(), "stop must be a string or an array of strings")
		}
	}
	return out, nil
}

func (p *Gemini) url(model, method string) string {
	return p.baseURL + "/models/" + url.PathEscape(model) + ":" + method
}

func (p *Gemini) Chat(ctx context.Context, req *api.ChatRequest) (*api.ChatResponse, error) {
	greq, perr := p.translate(req)
	if perr != nil {
		return nil, perr
	}
	resp, err := post(ctx, p.client, p.Name(), p.url(req.Model, "generateContent"), p.hdr, greq)
	if err != nil {
		return nil, err
	}
	var gr geminiResponse
	if err := decode(ctx, p.Name(), resp, &gr); err != nil {
		return nil, err
	}
	text, finish := gr.text()
	return &api.ChatResponse{
		ID:      gr.ResponseID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []api.Choice{{
			Message:      api.Message{Role: "assistant", Content: api.StringContent(text)},
			FinishReason: geminiFinish(finish),
		}},
		Usage: gr.usage(),
	}, nil
}

// ChatStream uses streamGenerateContent?alt=sse. Each event is a full
// GenerateContentResponse carrying the next text increment; the last one has
// finishReason and complete usageMetadata. There is no end marker: the
// stream simply ends, so EOF is the normal end here.
func (p *Gemini) ChatStream(ctx context.Context, req *api.ChatRequest) (Stream, error) {
	greq, perr := p.translate(req)
	if perr != nil {
		return nil, perr
	}
	resp, err := post(ctx, p.client, p.Name(), p.url(req.Model, "streamGenerateContent")+"?alt=sse", sseHeaders(p.hdr), greq)
	if err != nil {
		return nil, err
	}
	first, finished := true, false // owned by the stream's single reader
	s := newSSEStream(ctx, p.Name(), resp, func(_ string, data []byte) (*api.ChatChunk, error) {
		if finished {
			return nil, io.EOF
		}
		var gr geminiResponse
		if err := json.Unmarshal(data, &gr); err != nil {
			return nil, &ProviderError{Provider: p.Name(), Retryable: true, Message: "decode event: " + err.Error()}
		}
		text, reason := gr.text()
		role := ""
		if first {
			role, first = "assistant", false
		}
		c := api.TextChunk(role, text, geminiFinish(reason))
		if reason != "" {
			finished = true
			c.Usage = gr.usage()
		}
		return c, nil
	})
	s.endOnEOF = true
	return s, nil
}
