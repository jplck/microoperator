//go:build linux

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestAzureCredentialsConcurrentResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "AzureCLICredential")
	t.Setenv("PATH", root)
	data, err := json.Marshal(map[string]any{"accessToken": fixtureProviderSecret, "expires_on": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "az"), []byte("#!/bin/sh\nprintf '%s' '"+string(data)+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var credentials Credentials
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 8)
	for range cap(results) {
		go func() {
			token, err := credentials.Token(ctx, state.ProviderConfig{Adapter: "azure-openai"}, nil)
			if err == nil && token != fixtureProviderSecret {
				err = errors.New("Azure credential returned the wrong token")
			}
			results <- err
		}()
	}
	for range cap(results) {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

func TestAzureCredentialsSanitizeCLIErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "AzureCLICredential")
	t.Setenv("PATH", root)
	script := "#!/bin/sh\nprintf '" + fixtureProviderSecret + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(root, "az"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var credentials Credentials
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token, err := credentials.Token(ctx, state.ProviderConfig{Adapter: "azure-openai"}, nil)
	if err == nil || token != "" || !strings.Contains(err.Error(), "az login") ||
		strings.Contains(err.Error(), fixtureProviderSecret) {
		t.Fatal("Azure CLI error was not surfaced safely")
	}
}

func TestCredentialsModesAndCancellation(t *testing.T) {
	// Selecting an invalid Azure chain must not affect local/API-key providers.
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "invalid-fixture-credential")
	var credentials Credentials
	for _, tc := range []struct {
		adapter, value, want string
		invalid              bool
	}{
		{"ollama", "", "", false},
		{"openai-chat-completions", fixtureProviderSecret, fixtureProviderSecret, false},
		{"openai-chat-completions", "", "", true},
		{"openai-chat-completions", "bad\r\nkey", "", true},
		{"azure-openai", fixtureProviderSecret, "", true},
		{"unknown", fixtureProviderSecret, "", true},
	} {
		t.Run(tc.adapter+"/"+tc.value, func(t *testing.T) {
			key, err := credentials.Token(context.Background(), state.ProviderConfig{Adapter: tc.adapter, APIKeyEnv: "FIXTURE_KEY"}, func(name string) (string, bool) {
				if tc.adapter != "openai-chat-completions" || name != "FIXTURE_KEY" {
					t.Fatal("unexpected credential lookup")
				}
				return tc.value, tc.value != ""
			})
			if (err != nil) != tc.invalid || key != tc.want {
				t.Fatalf("credential resolution: key matched=%v, err=%v", key == tc.want, err)
			}
			if err != nil && strings.Contains(err.Error(), fixtureProviderSecret) {
				t.Fatal("credential error leaked secret")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := credentials.Token(ctx, state.ProviderConfig{Adapter: "azure-openai"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled credential request: %v", err)
	}
}
