package mockupstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMockInjection(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want int
	}{
		{"healthy", Config{}, 200},
		{"always 500", Config{ErrorRate: 1}, 500},
		{"always 429", Config{RateLimitRate: 1}, 429},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.cfg)
			rec := httptest.NewRecorder()
			body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == 429 && rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			if s.Requests() != 1 {
				t.Errorf("requests = %d", s.Requests())
			}
		})
	}
}
