//go:build linux

package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jplck/microoperator/internal/state"
)

type Credentials struct {
	once  sync.Once
	azure *azidentity.DefaultAzureCredential
	err   error
}

func (c *Credentials) Token(ctx context.Context, provider state.ProviderConfig, lookup func(string) (string, bool)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch provider.Adapter {
	case "ollama":
		return "", nil
	case "openai-chat-completions":
		key, ok := lookup(provider.APIKeyEnv)
		if !ok || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
			return "", errors.New("provider credential unavailable before request")
		}
		return key, nil
	case "azure-openai":
		c.once.Do(func() {
			c.azure, c.err = azidentity.NewDefaultAzureCredential(nil)
		})
		if c.err != nil {
			return "", errors.New("Azure DefaultAzureCredential initialization failed before request; check AZURE_* settings")
		}
		token, err := c.azure.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://ai.azure.com/.default"}})
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// SDK/CLI errors can include tokens or response bodies; never persist them.
		if err != nil {
			return "", errors.New("Azure DefaultAzureCredential authentication failed before request; run az login and check Azure credential configuration")
		}
		if strings.TrimSpace(token.Token) == "" || strings.ContainsAny(token.Token, "\r\n") || !token.ExpiresOn.After(time.Now()) {
			return "", errors.New("Azure DefaultAzureCredential returned an invalid or expired token before request")
		}
		return token.Token, nil
	default:
		return "", errors.New("unsupported provider adapter before request")
	}
}
