//go:build integration && linux

package daemon

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jplck/microoperator/internal/state"
)

func TestOperatorConfiguredProviderTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int64
		slow    bool
	}{
		{"response after legacy timeout", 90, true},
		{"configured deadline", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "" {
					t.Error("local provider routing or credentials changed")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				io.Copy(io.Discard, r.Body)
				if tc.slow {
					timer := time.NewTimer(61 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						completion(w, "slow local result")
					case <-r.Context().Done():
						t.Error("provider canceled before the delayed response")
					}
				} else {
					<-r.Context().Done()
				}
			}))
			t.Cleanup(server.Close)
			cfg := fixtureConfiguration(t)
			cfg.Providers["primary"] = state.ProviderConfig{Adapter: "ollama", BaseURL: server.URL + "/v1", TimeoutSeconds: tc.timeout}
			filename := filepath.Join(t.TempDir(), "daemon.json")
			writeFixtureConfiguration(t, filename, cfg)
			d := startDaemonFixture(t, filename, fixtureControlToken, fixtureProviderEnv+"=")
			record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
				state.CreateSystemCommand{Name: "Local timeout fixture", Goal: fixtureGoal()}, http.StatusCreated)
			daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "start",
				state.StartSystemCommand{ExpectedRevision: 1}, http.StatusAccepted)
			record = waitSystemFor(t, d, record.ID, 90*time.Second, func(s state.SystemRecord) bool { return s.State == "inactive" })
			if record.Execution.Attempts != 1 || requests.Load() != 1 {
				t.Fatalf("provider was retried: %+v; requests=%d", record.Execution, requests.Load())
			}
			if tc.slow {
				if record.Execution.State != "completed" || record.Execution.Response != "slow local result" ||
					record.UsedTokens != 18 || record.ReservedTokens != 0 {
					t.Fatalf("configured timeout did not allow slow inference: %+v", record)
				}
			} else {
				if record.Execution.State != "unknown" || !strings.Contains(record.Execution.Reason, "deadline exceeded") ||
					record.UsedTokens != 0 || record.ReservedTokens != record.Execution.Reservation || record.ReservedTokens == 0 {
					t.Fatalf("timeout was hidden or refunded: %+v", record)
				}
				status, data := daemonRequest(t, d, fixtureControlToken, "GET", "/v1/systems/"+record.ID+"/activity", "", nil)
				var activity state.ActivitySnapshot
				if err := json.Unmarshal(data, &activity); err != nil || status != http.StatusOK {
					t.Fatalf("activity: status=%d err=%v", status, err)
				}
				found := false
				for _, event := range activity.Events {
					if event.Type == "task.ready" {
						found = event.State == "dead" && event.Reason == record.Execution.Reason
					}
				}
				if !found {
					t.Fatalf("activity hid provider failure: %+v", activity.Events)
				}
			}
			d.stop(t, false)
			restarted := startDaemonFixture(t, filename, fixtureControlToken, fixtureProviderEnv+"=")
			saved := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, http.StatusOK)
			if saved.State != "inactive" || saved.UsedTokens != record.UsedTokens || saved.ReservedTokens != record.ReservedTokens ||
				saved.Execution.State != record.Execution.State || saved.Execution.Attempts != 1 || requests.Load() != 1 {
				t.Fatalf("restart changed timeout accounting or replayed work: %+v", saved)
			}
			restarted.stop(t, false)
		})
	}
}

func TestOperatorOllamaAndAzureCredentials(t *testing.T) {
	for _, adapter := range []string{"ollama", "azure-openai"} {
		t.Run(adapter, func(t *testing.T) {
			var requests atomic.Int64
			var expectedToken atomic.Value
			expectedToken.Store("")
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				path, capName := "/v1/chat/completions", "max_tokens"
				if adapter == "azure-openai" {
					path, capName = "/openai/v1/chat/completions", "max_completion_tokens"
				}
				token := expectedToken.Load().(string)
				if r.Method != "POST" || r.URL.Path != path || r.URL.RawQuery != "" ||
					(token == "" && len(r.Header.Values("Authorization")) != 0) ||
					(token != "" && r.Header.Get("Authorization") != "Bearer "+token) {
					t.Error("provider request had wrong routing or authentication")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if string(request[capName]) != "1024" || string(request["model"]) != `"fixture-model"` ||
					(capName == "max_tokens" && request["max_completion_tokens"] != nil) ||
					(capName == "max_completion_tokens" && request["max_tokens"] != nil) {
					t.Error("provider model or output cap changed")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				text := "local result"
				if token != "" {
					text = "Azure result " + token
				}
				if string(request["stream"]) == "true" {
					streamCompletion(w, text)
				} else {
					completion(w, text)
				}
			})
			var server *httptest.Server
			if adapter == "azure-openai" {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			cfg := fixtureConfiguration(t)
			endpoint := server.URL + "/v1"
			env := []string{fixtureProviderEnv + "="}
			root := t.TempDir()
			responseFile := filepath.Join(root, "token.json")
			callsFile := filepath.Join(root, "cli-calls")
			tokens := []string{"fixture-azure-token-one-not-real", "fixture-azure-token-two-not-real"}
			writeToken := func(token string, expires time.Time) {
				t.Helper()
				data, err := json.Marshal(map[string]any{"accessToken": token, "expires_on": expires.Unix()})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(responseFile, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if adapter == "azure-openai" {
				endpoint = server.URL + "/openai/v1"
				certFile := filepath.Join(root, "ca.pem")
				if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				script := `#!/bin/sh
if [ "$*" != "account get-access-token -o json --resource https://ai.azure.com" ]; then
  printf 'unexpected Azure CLI arguments\n' >&2
  exit 1
fi
printf 'token\n' >> "$MICROOPERATOR_TEST_AZ_CALLS"
exec /bin/cat "$MICROOPERATOR_TEST_AZ_RESPONSE"
`
				if err := os.WriteFile(filepath.Join(root, "az"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				env = append(env, "PATH="+root, "HOME="+root, "AZURE_CONFIG_DIR="+root,
					"AZURE_TOKEN_CREDENTIALS=AzureCLICredential", "SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+root,
					"MICROOPERATOR_TEST_AZ_RESPONSE="+responseFile, "MICROOPERATOR_TEST_AZ_CALLS="+callsFile)
			}
			cfg.Providers["primary"] = state.ProviderConfig{Adapter: adapter, BaseURL: endpoint}
			quota := cfg.QuotaGroups["account"]
			quota.BurstRequests = 8
			cfg.QuotaGroups["account"] = quota
			filename := filepath.Join(root, "daemon.json")
			writeFixtureConfiguration(t, filename, cfg)
			d := startDaemonFixture(t, filename, fixtureControlToken, env...)
			record := daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems", "create",
				state.CreateSystemCommand{Name: "research", Goal: fixtureGoal()}, http.StatusCreated)
			if _, err := os.Stat(callsFile); !os.IsNotExist(err) || requests.Load() != 0 {
				t.Fatal("startup or system creation acquired credentials or called a model")
			}
			for i, stream := range []bool{false, true} {
				if adapter == "azure-openai" {
					expectedToken.Store(tokens[i])
					writeToken(tokens[i], time.Now().Add(time.Hour))
				}
				goal := "first goal"
				if stream {
					goal = "second goal"
				}
				daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", strings.ReplaceAll(goal, " ", "-"),
					state.StartSystemCommand{ExpectedRevision: 1, Goal: &goal, Stream: stream}, http.StatusAccepted)
				record = waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
				want := "local result"
				if adapter == "azure-openai" {
					want = "Azure result [redacted]"
				}
				if record.Execution.State != "completed" || record.Execution.Response != want ||
					record.UsedTokens != int64(i+1)*18 || record.ReservedTokens != 0 || requests.Load() != int64(i+1) {
					t.Fatalf("provider completion/accounting: %+v; requests=%d", record, requests.Load())
				}
			}
			if adapter == "azure-openai" {
				writeToken(tokens[1], time.Unix(1, 0))
				goal := "expired credentials"
				daemonSystem(t, d, fixtureControlToken, "POST", "/v1/systems/"+record.ID+"/start", "expired",
					state.StartSystemCommand{ExpectedRevision: 1, Goal: &goal}, http.StatusAccepted)
				record = waitSystem(t, d, record.ID, func(s state.SystemRecord) bool { return s.State == "inactive" })
				if record.Execution.State != "failed" || !strings.Contains(record.Execution.Reason, "expired token") ||
					record.UsedTokens != 36 || record.ReservedTokens != 0 || requests.Load() != 2 {
					t.Fatalf("expired Azure token was sent or charged: %+v", record)
				}
				calls, err := os.ReadFile(callsFile)
				if err != nil || string(calls) != "token\ntoken\ntoken\n" {
					t.Fatalf("Azure credential did not refresh per admitted request: %q, %v", calls, err)
				}
			}
			d.stop(t, false)
			restarted := startDaemonFixture(t, filename, fixtureControlToken, env...)
			saved := daemonSystem(t, restarted, fixtureControlToken, "GET", "/v1/systems/"+record.ID, "", nil, http.StatusOK)
			if saved.UsedTokens != 36 || saved.ReservedTokens != 0 || requests.Load() != 2 {
				t.Fatalf("provider restart lost usage or replayed work: %+v", saved)
			}
			restarted.stop(t, false)
			if err := filepath.WalkDir(cfg.DataDir, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || !entry.Type().IsRegular() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, token := range tokens {
					if bytes.Contains(data, []byte(token)) || strings.Contains(d.stderr.String(), token) ||
						strings.Contains(restarted.stderr.String(), token) {
						t.Error("Azure token leaked into durable state, artifacts, or daemon diagnostics")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
