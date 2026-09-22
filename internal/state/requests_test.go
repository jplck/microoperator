package state

import (
	"encoding/json"
	"testing"
)

func TestProviderRequestTokenCaps(t *testing.T) {
	for _, adapter := range []string{"openai-chat-completions", "ollama", "azure-openai"} {
		t.Run(adapter, func(t *testing.T) {
			cfg := fixtureConfiguration(t)
			provider := cfg.Providers["primary"]
			provider.Adapter = adapter
			cfg.Providers["primary"] = provider
			grants, err := cfg.GrantsFor(*cfg.Bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			body, reservation, err := ModelRequest(cfg, SystemRecord{Configuration: *cfg.Bootstrap, Grants: grants}, "hello", true)
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]json.RawMessage
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			cap, absent := "max_tokens", "max_completion_tokens"
			if adapter == "azure-openai" {
				cap, absent = absent, cap
			}
			if string(request[cap]) != "1024" || request[absent] != nil ||
				string(request["model"]) != `"fixture-model"` || string(request["stream_options"]) != `{"include_usage":true}` {
				t.Fatalf("wrong provider request: %s", body)
			}
			if want := (int64(len(body))*120+99)/100 + 1024; reservation != want {
				t.Fatalf("reservation = %d; want %d for actual serialized request", reservation, want)
			}
		})
	}
}
