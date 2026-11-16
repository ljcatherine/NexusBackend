package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockProviderPS5 is a fake Provider used to prove the interface shape.
type mockProviderPS5 struct{ engines []string }

var _ Provider = (*mockProviderPS5)(nil)

func (m *mockProviderPS5) Name() string { return nexusAIPS5 }

func (m *mockProviderPS5) Health(context.Context) ProviderHealth {
	return ProviderHealth{Online: true, Ready: true}
}

func (m *mockProviderPS5) Chat(_ context.Context, _ ProviderRequest, w http.ResponseWriter) error {
	w.WriteHeader(http.StatusOK)
	return nil
}

func (m *mockProviderPS5) Media(_ context.Context, kind string, _ ProviderRequest) (MediaResult, error) {
	return MediaResult{ContentType: "image/png", Body: []byte(kind)}, nil
}

func (m *mockProviderPS5) EnsureEngine(_ context.Context, engine string) error {
	m.engines = append(m.engines, engine)
	return nil
}

func TestProviderNameConstant(t *testing.T) {
	if nexusAIPS5 != "nexusai-ps5" {
		t.Fatalf("nexusAIPS5 = %q, want %q", nexusAIPS5, "nexusai-ps5")
	}
}

func TestProviderFakeSatisfiesInterface(t *testing.T) {
	ctx := context.Background()
	var p Provider = &mockProviderPS5{}

	if got := p.Name(); got != nexusAIPS5 {
		t.Fatalf("Name() = %q, want %q", got, nexusAIPS5)
	}
	if h := p.Health(ctx); !h.Online || !h.Ready {
		t.Fatalf("Health() = %+v, want online and ready", h)
	}
	rec := httptest.NewRecorder()
	if err := p.Chat(ctx, ProviderRequest(`{}`), rec); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	res, err := p.Media(ctx, "image", ProviderRequest(`{}`))
	if err != nil || res.ContentType != "image/png" {
		t.Fatalf("Media() = %+v, %v", res, err)
	}
	if err := p.EnsureEngine(ctx, "llm"); err != nil {
		t.Fatalf("EnsureEngine() error = %v", err)
	}
}
