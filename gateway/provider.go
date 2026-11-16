package main

import (
	"context"
	"encoding/json"
	"net/http"
)

// nexusAIPS5 is the name of the PS5 NexusAI provider.
const nexusAIPS5 = "nexusai-ps5"

// ProviderRequest is the raw JSON body forwarded to a provider unchanged.
type ProviderRequest = json.RawMessage

// ProviderHealth is the provider state the gateway reports to callers.
type ProviderHealth struct {
	Online bool   `json:"online"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// MediaResult is a finished media job: its content type and encoded bytes.
type MediaResult struct {
	ContentType string
	Body        []byte
}

// Provider is a backend that serves chat and media requests.
type Provider interface {
	Name() string
	Health(ctx context.Context) ProviderHealth
	Chat(ctx context.Context, req ProviderRequest, w http.ResponseWriter) error
	Media(ctx context.Context, kind string, req ProviderRequest) (MediaResult, error)
	EnsureEngine(ctx context.Context, engine string) error
}
