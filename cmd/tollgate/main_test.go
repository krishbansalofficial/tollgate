package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishbansalofficial/tollgate/internal/api"
	"github.com/krishbansalofficial/tollgate/internal/config"
	"github.com/krishbansalofficial/tollgate/internal/mockupstream"
	"github.com/krishbansalofficial/tollgate/internal/router"
)

// TestOllamaFallbackWiring exercises the real wiring: env config ->
// buildProviders -> the shipped catalog -> router. Groq fails every request;
// the configured fallback model must be served by the Ollama provider.
func TestOllamaFallbackWiring(t *testing.T) {
	groq := httptest.NewServer(mockupstream.New(mockupstream.Config{ErrorRate: 1}).Handler())
	defer groq.Close()
	ollama := mockupstream.New(mockupstream.Config{})
	ollamaSrv := httptest.NewServer(ollama.Handler())
	defer ollamaSrv.Close()

	env := map[string]string{
		"TOLLGATE_DATABASE_URL":    "postgres://unused",
		"GROQ_API_KEY":             "test",
		"TOLLGATE_GROQ_BASE_URL":   groq.URL + "/v1",
		"TOLLGATE_OLLAMA_BASE_URL": ollamaSrv.URL + "/v1",
		"TOLLGATE_FALLBACK_MODEL":  "ollama/llama3.2:3b",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	provs := buildProviders(cfg)
	if _, ok := provs["ollama"]; !ok {
		t.Fatal("ollama provider not built from TOLLGATE_OLLAMA_BASE_URL")
	}
	cat, err := router.LoadCatalog("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Models[cfg.FallbackModel]; !ok {
		t.Fatalf("fallback %q missing from the shipped catalog", cfg.FallbackModel)
	}
	rcfg := router.DefaultConfig()
	rcfg.Fallback, rcfg.AttemptTimeout, rcfg.MaxAttempts = cfg.FallbackModel, 5*time.Second, 1
	rt := router.New(rcfg, cat, provs, nil)

	req := &api.ChatRequest{Model: "groq/llama-3.1-8b-instant", Messages: []api.Message{{Role: "user", Content: api.StringContent("hi")}}}
	res, err := rt.Chat(context.Background(), req)
	if err != nil || res.Model != "ollama/llama3.2:3b" || res.Provider != "ollama" || ollama.Requests() != 1 {
		t.Fatalf("res=%+v err=%v ollama requests=%d", res, err, ollama.Requests())
	}
}
