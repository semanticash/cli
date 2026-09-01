package commands

import (
	"testing"

	"github.com/semanticash/cli/internal/service"
)

func TestSessionIdentity(t *testing.T) {
	withModel := service.SessionSummary{Provider: "cursor", Model: "grok-4.6"}
	if got := sessionIdentity(withModel); got != "cursor (grok-4.6)" {
		t.Fatalf("identity = %q, want provider and model", got)
	}

	withoutModel := service.SessionSummary{Provider: "codex"}
	if got := sessionIdentity(withoutModel); got != "codex" {
		t.Fatalf("identity = %q, want provider only", got)
	}
}

func TestSessionTokenSummary(t *testing.T) {
	tests := []struct {
		name string
		in   service.SessionSummary
		want string
	}{
		{name: "unavailable", in: service.SessionSummary{}, want: "tok unavailable"},
		{name: "measured zero", in: service.SessionSummary{TokenUsageValid: true}, want: "tok 0/0"},
		{
			name: "measured with cache",
			in: service.SessionSummary{
				TokenUsageValid: true,
				TokensIn:        20260,
				TokensOut:       2551,
				TokensCached:    114048,
			},
			want: "tok 20k/2.6k (+114k cached)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionTokenSummary(tt.in); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}
