# ADR 0005: Talk to providers with raw net/http, not vendor SDKs

Status: accepted (2026-11)

## Context

Every provider response is translated to the OpenAI schema anyway. Vendor SDKs ship their own retries, timeouts and connection pools.

## Decision

One `http.Client` per provider with a tuned transport (100 idle conns/host), deadlines from the request context, and hand-written translation for Anthropic and Gemini. OpenAI-compatible providers (Groq, Ollama) share one adapter.

## Consequences

The router's retries, budget and circuit breaker are the only retry layer: SDK-level retries would multiply attempts invisibly during outages. Streaming parsing and error classification are explicit and testable with httptest. Cost: tracking API changes ourselves (e.g. Anthropic's 2026-10-21 version removing sampling parameters).
