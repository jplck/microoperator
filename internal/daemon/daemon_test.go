//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

func fixtureJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func controlRequest(handler http.Handler, method, path, key, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeSystemResponse(t *testing.T, response *httptest.ResponseRecorder, status int) state.SystemRecord {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var record state.SystemRecord
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestControlAuthorizationValidationAndRevisions(t *testing.T) {
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	var logs bytes.Buffer
	handler := newControlHandler(store, cfg, configID, fixtureControlToken, log.New(&logs, "", 0), nil)
	for _, route := range []string{"/v1/health", "/v1/launch-configurations", "/v1/systems", "/v1/unknown"} {
		response := controlRequest(handler, "GET", route, "", "", "wrong")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated route %s: %d", route, response.Code)
		}
	}
	request := httptest.NewRequest("POST", "http://localhost/v1/systems", strings.NewReader(`{"launch":"research"}`))
	request.Header.Set("Authorization", "Bearer "+fixtureControlToken)
	request.Header.Set("Origin", "https://example.invalid")
	request.Header.Set("X-Principal", localAdministrator)
	request.Header.Set("X-System-ID", "spoofed")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("browser-origin request: %d", response.Code)
	}
	for _, tc := range []struct{ name, key, body string }{
		{"missing key", "", `{"launch":"research"}`},
		{"invalid key", "bad key", `{"launch":"research"}`},
		{"unknown launch", "unknown", `{"launch":"missing"}`},
		{"supplied identity", "identity", `{"launch":"research","system_id":"caller-chosen"}`},
		{"supplied scope", "scope", `{"launch":"research","owner":"other-user"}`},
		{"implicit execution", "start", `{"launch":"research","start":true}`},
		{"duplicate fields", "duplicate", `{"launch":"research","launch":"missing"}`},
		{"incorrect casing", "case", `{"Launch":"research"}`},
		{"oversized", "big", strings.Repeat(" ", protocol.MaxFrame+1)},
		{"invalid JSON", "invalid", `{"api_key":"` + fixtureProviderSecret + `"}`},
		{"empty goal", "goal", `{"launch":"research","goal":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := controlRequest(handler, "POST", "/v1/systems", tc.key, tc.body, fixtureControlToken)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), fixtureProviderSecret) ||
				strings.Contains(response.Body.String(), fixtureControlToken) {
				t.Fatal("credential echoed by error response")
			}
		})
	}
	assertCount(t, store, "systems", 0)
	body := `{"launch":"research","goal":"A pending fixture goal."}`
	first := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems", "create", body, fixtureControlToken), http.StatusCreated)
	second := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems", "create-two", body, fixtureControlToken), http.StatusCreated)
	replay := decodeSystemResponse(t, controlRequest(handler, "POST", "/v1/systems", "create", body, fixtureControlToken), http.StatusCreated)
	if !reflect.DeepEqual(first, replay) || first.ID == second.ID || first.Goal.State != "pending" {
		t.Fatal("API creation or idempotency changed identity")
	}
	get := decodeSystemResponse(t, controlRequest(handler, "GET", "/v1/systems/"+first.ID, "", "", fixtureControlToken), http.StatusOK)
	if !reflect.DeepEqual(get, first) {
		t.Fatal("inspection differs from persisted creation")
	}
	next := first.Configuration
	next.Operator.Prompt = "Explicitly changed."
	next.Operator.Tools = []string{}
	update := fixtureJSON(t, state.ReviseSystemCommand{ExpectedRevision: first.Revision, Configuration: &next})
	path := "/v1/systems/" + first.ID + "/configuration"
	revised := decodeSystemResponse(t, controlRequest(handler, "PUT", path, "revise", update, fixtureControlToken), http.StatusOK)
	if revised.Revision != 2 || len(revised.Grants.OperatorTools) != 0 || revised.State != "inactive" {
		t.Fatalf("revision/grants/state: %+v", revised)
	}
	if response := controlRequest(handler, "PUT", path, "stale", update, fixtureControlToken); response.Code != http.StatusConflict {
		t.Fatalf("stale revision status %d: %s", response.Code, response.Body.String())
	}
	if response := controlRequest(handler, "POST", "/v1/systems", "create", `{"launch":"research"}`, fixtureControlToken); response.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict status %d", response.Code)
	}
	if response := controlRequest(handler, "POST", "/v1/systems/"+first.ID+"/start", "start", `{}`, fixtureControlToken); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("execution accepted without a lifecycle owner: %d", response.Code)
	}
	foreign, err := store.CreateSystem(context.Background(), "another-principal", "foreign", state.CreateSystemCommand{Launch: "research"}, cfg, configID)
	if err != nil {
		t.Fatal(err)
	}
	if response := controlRequest(handler, "GET", "/v1/systems/"+foreign.ID, "", "", fixtureControlToken); response.Code != http.StatusNotFound {
		t.Fatalf("foreign system exposed: %d", response.Code)
	}
	for _, route := range []string{"/v1/systems", "/v1/health", "/v1/launch-configurations"} {
		response := controlRequest(handler, "GET", route, "", "", fixtureControlToken)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), fixtureProviderSecret) ||
			strings.Contains(response.Body.String(), fixtureControlToken) || strings.Contains(response.Body.String(), foreign.ID) {
			t.Fatalf("unsafe inspection %s: %s", route, response.Body.String())
		}
	}
	if strings.Contains(logs.String(), fixtureProviderSecret) || strings.Contains(logs.String(), fixtureControlToken) {
		t.Fatal("credential logged")
	}
}

func TestControlPaginationAndStorageFailures(t *testing.T) {
	cfg := fixtureConfiguration(t)
	store, configID := fixtureStore(t, cfg)
	handler := newControlHandler(store, cfg, configID, fixtureControlToken, log.New(&bytes.Buffer{}, "", 0), nil)
	for i := 0; i < 21; i++ {
		if _, err := store.CreateSystem(context.Background(), localAdministrator, fmt.Sprintf("create-%d", i),
			state.CreateSystemCommand{Launch: "research"}, cfg, configID); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Systems []state.SystemRecord `json:"systems"`
		Next    string               `json:"next"`
	}
	response := controlRequest(handler, "GET", "/v1/systems", "", "", fixtureControlToken)
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Systems) != 20 || page.Next == "" {
		t.Fatalf("first page: %+v", page)
	}
	firstIDs := make(map[string]bool)
	for _, record := range page.Systems {
		firstIDs[record.ID] = true
	}
	response = controlRequest(handler, "GET", "/v1/systems?after="+page.Next, "", "", fixtureControlToken)
	page.Next = ""
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Systems) != 1 || page.Next != "" || firstIDs[page.Systems[0].ID] {
		t.Fatalf("second page: %+v", page)
	}
	for _, query := range []string{"?after=bad", "?other=value", "?after=a&after=b", "?after=%ZZ"} {
		if response := controlRequest(handler, "GET", "/v1/systems"+query, "", "", fixtureControlToken); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid cursor accepted: %s", query)
		}
	}
	if _, err := testDB(t, store).Exec(`CREATE TRIGGER fail_api_audit BEFORE INSERT ON audit
		BEGIN SELECT RAISE(ABORT, 'private storage diagnostic'); END;`); err != nil {
		t.Fatal(err)
	}
	response = controlRequest(handler, "POST", "/v1/systems", "storage-failure", `{"launch":"research"}`, fixtureControlToken)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private storage diagnostic") {
		t.Fatalf("storage failure response: %d %s", response.Code, response.Body.String())
	}
	assertCount(t, store, "systems", 21)
}

func TestStateFilesArePrivateAndNeverFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockStateDirectory(root); err == nil {
		lock.Close()
		t.Fatal("non-private data directory accepted")
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := lockStateDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if another, err := lockStateDirectory(root); err == nil {
		another.Close()
		t.Fatal("second daemon lock accepted")
	}
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	if store, err := openTestStore(t, context.Background(), filepath.Join(root, "state.db")); err == nil {
		store.Close()
		t.Fatal("symlinked database accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "not a database" {
		t.Fatal("database symlink target changed")
	}
	if err := os.WriteFile(filepath.Join(root, "control.sock"), []byte("unrelated fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenControl(root); err == nil {
		listener.Close()
		t.Fatal("existing regular control.sock was replaced")
	}
	if err := os.Remove(filepath.Join(root, "control.sock")); err != nil {
		t.Fatal(err)
	}
	active, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: filepath.Join(root, "control.sock")})
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if listener, err := listenControl(root); err == nil {
		listener.Close()
		t.Fatal("an active socket without this daemon's lock was replaced")
	}
	if _, err := os.Stat(filepath.Join(root, "control.sock")); err != nil {
		t.Fatalf("active socket was unlinked: %v", err)
	}
}
