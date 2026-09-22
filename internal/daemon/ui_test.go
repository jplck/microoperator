//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/jplck/microoperator/internal/state"
	webui "github.com/jplck/microoperator/internal/ui"
)

const fixtureUIToken = "fixture-ui-token-not-a-real-secret-123456789"

func fixtureUI(t *testing.T, cfg state.Configuration) (*httptest.Server, *executionEngine) {
	t.Helper()
	engine, id := fixtureTeamEngine(t, cfg)
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	backend := &http.Server{Handler: newControlHandler(engine.store, cfg, id, fixtureControlToken, engine.logger, engine)}
	done := make(chan error, 1)
	go func() { done <- backend.Serve(listener) }()
	t.Cleanup(func() { backend.Close(); <-done })
	client := webui.NewClient(listener.Addr().String())
	t.Cleanup(client.CloseIdleConnections)
	ui := httptest.NewUnstartedServer(nil)
	ui.Config.Handler = webui.NewHandler(client, fixtureControlToken, fixtureUIToken, ui.Listener.Addr().String(), engine.logger)
	ui.Start()
	t.Cleanup(ui.Close)
	return ui, engine
}

func uiResponse(t *testing.T, ui *httptest.Server, method, path string, values url.Values, origin string) (int, string) {
	t.Helper()
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	request, err := http.NewRequest(method, ui.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("operator", fixtureUIToken)
	if values != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := ui.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), fixtureControlToken) || strings.Contains(string(data), fixtureUIToken) {
		t.Fatal("UI exposed a credential")
	}
	return response.StatusCode, string(data)
}

func hiddenUIValue(t *testing.T, body, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("missing form field %s", name)
	}
	return match[1]
}

func uiCommandForm(t *testing.T, body, target string) url.Values {
	t.Helper()
	page, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(node *html.Node, name string) string {
		for _, field := range node.Attr {
			if field.Key == name {
				return field.Val
			}
		}
		return ""
	}
	var result url.Values
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "form" {
			form := url.Values{}
			var fields func(*html.Node)
			fields = func(child *html.Node) {
				if name := attr(child, "name"); name != "" {
					if child.Data == "input" {
						form.Set(name, attr(child, "value"))
					} else if child.Data == "textarea" {
						text := ""
						if child.FirstChild != nil {
							text = child.FirstChild.Data
						}
						form.Set(name, text)
					}
				}
				for nested := child.FirstChild; nested != nil; nested = nested.NextSibling {
					fields(nested)
				}
			}
			fields(node)
			if form.Get("path") == target {
				result = form
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(page)
	if result == nil {
		t.Fatalf("missing command form for %s", target)
	}
	return result
}

func TestUIAuthenticationCSRFAndDurableCommands(t *testing.T) {
	ui, engine := fixtureUI(t, knowledgeConfiguration(t))
	response, err := ui.Client().Get(ui.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("UI allowed unauthenticated browsing")
	}
	if policy := response.Header.Get("Referrer-Policy"); policy != "same-origin" {
		t.Fatalf("native forms need a same-origin Origin without cross-origin referrers; policy=%q", policy)
	}
	status, body := uiResponse(t, ui, "GET", "/", nil, "")
	if status != 200 {
		t.Fatal(body)
	}
	csrf, key := hiddenUIValue(t, body, "csrf"), hiddenUIValue(t, body, "key")
	form := url.Values{"kind": {"create"}, "path": {"/v1/systems"}, "method": {"POST"}, "return": {"/"}, "key": {key}, "csrf": {csrf}, "name": {"research"}, "goal": {"<script>alert('fixture')</script>"}}
	for _, origin := range []string{"http://attacker.invalid", "null", strings.Replace(ui.URL, "http:", "https:", 1)} {
		if status, _ := uiResponse(t, ui, "POST", "/command", form, origin); status != 403 {
			t.Fatalf("cross-origin command accepted: %s", origin)
		}
	}
	form.Set("csrf", "forged")
	for _, origin := range []string{"", ui.URL} {
		if status, _ := uiResponse(t, ui, "POST", "/command", form, origin); status != 403 {
			t.Fatal("forged CSRF command accepted")
		}
	}
	assertCount(t, engine.store, "systems", 0)
	form.Set("csrf", csrf)
	for _, origin := range []string{ui.URL, ""} {
		status, body := uiResponse(t, ui, "POST", "/command", form, origin)
		if status != 201 || strings.Contains(body, "<script>") {
			t.Fatalf("command failed or HTML escaped incorrectly: %d %s", status, body)
		}
	}
	assertCount(t, engine.store, "systems", 1)
	var id string
	if err := testDB(t, engine.store).QueryRow(`SELECT system_id FROM systems`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/systems/" + id, "/systems/" + id + "/activity", "/systems/" + id + "/tools", "/systems/" + id + "/memory", "/systems/" + id + "/schedules", "/systems/" + id + "/learning", "/systems/" + id + "/learning-checks", "/systems/" + id + "/learning-feedback", "/broker", "/tools"} {
		if status, body := uiResponse(t, ui, "GET", route, nil, ""); status != 200 {
			t.Fatalf("%s: %d %s", route, status, body)
		}
	}
	start := url.Values{"kind": {"start"}, "path": {"/v1/systems/" + id + "/start"}, "method": {"POST"}, "return": {"/systems/" + id}, "key": {"ui-start"}, "csrf": {csrf}, "expected_revision": {"1"}, "lifetime_seconds": {"3600"}}
	if status, body := uiResponse(t, ui, "POST", "/command", start, ""); status != 202 {
		t.Fatalf("UI start: %s", body)
	}
	pause := url.Values{"kind": {"json"}, "path": {"/v1/systems/" + id + "/pause"}, "method": {"POST"}, "return": {"/systems/" + id}, "key": {"ui-pause"}, "csrf": {csrf}, "json": {"{}"}}
	if status, body := uiResponse(t, ui, "POST", "/command", pause, ""); status != 202 {
		t.Fatalf("UI pause: %s", body)
	}
	record, err := engine.store.GetSystem(context.Background(), localAdministrator, id)
	if err != nil || record.State != "paused" {
		t.Fatalf("UI did not control daemon state: %+v %v", record, err)
	}
	assertCount(t, engine.store, "model_attempts", 0)
	request, err := http.NewRequest("GET", ui.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "rebound.invalid"
	request.SetBasicAuth("operator", fixtureUIToken)
	response, err = ui.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("DNS-rebinding Host accepted")
	}
}

func TestUIGoalCreationWithoutTemplates(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			cfg := knowledgeConfiguration(t)
			if !configured {
				cfg.Bootstrap = nil
			}
			ui, engine := fixtureUI(t, cfg)
			status, page := uiResponse(t, ui, "GET", "/", nil, "")
			if status != http.StatusOK || strings.Contains(page, `name="launch"`) ||
				!strings.Contains(page, `name="name" maxlength="128" required`) ||
				!strings.Contains(page, "Default token budget:") || !strings.Contains(page, `name="constraints"`) {
				t.Fatalf("UI did not offer goal-driven creation: %d %s", status, page)
			}
			form := url.Values{
				"kind": {"create"}, "path": {"/v1/systems"}, "method": {"POST"}, "return": {"/"},
				"csrf": {hiddenUIValue(t, page, "csrf")}, "key": {hiddenUIValue(t, page, "key")},
				"name": {""}, "goal": {"Record without execution."},
				"constraints": {"Only use simulated data."}, "token_budget": {"40000"},
			}
			if status, _ := uiResponse(t, ui, "POST", "/command", form, ui.URL); status != http.StatusBadRequest {
				t.Fatal("missing name bypassed daemon validation")
			}
			assertCount(t, engine.store, "systems", 0)
			form.Set("name", "Paper trading experiment")
			if status, body := uiResponse(t, ui, "POST", "/command", form, ui.URL); status != http.StatusCreated {
				t.Fatalf("goal did not create a system: %d %s", status, body)
			}
			assertCount(t, engine.store, "systems", 1)
			assertCount(t, engine.store, "model_attempts", 0)
			records, _, err := engine.store.ListSystems(context.Background(), localAdministrator, "")
			if err != nil || len(records) != 1 || records[0].Configuration.Name != "Paper trading experiment" ||
				records[0].Configuration.Constraints != "Only use simulated data." || records[0].Configuration.Limits.TokenBudget != 40000 {
				t.Fatalf("creation form lost name/constraints/budget: %+v %v", records, err)
			}
		})
	}
}

func TestUITextAttachmentIsBoundedScopedAndIdempotent(t *testing.T) {
	ui, engine := fixtureUI(t, knowledgeConfiguration(t))
	var configID string
	if err := testDB(t, engine.store).QueryRow(`SELECT config_id FROM config_snapshots LIMIT 1`).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	e := knowledgeCall(t, engine, configID, "upload")
	_, body := uiResponse(t, ui, "GET", "/systems/"+e.SystemID, nil, "")
	csrf := hiddenUIValue(t, body, "csrf")
	for _, size := range []int{10, 10, 3073} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for name, value := range map[string]string{"csrf": csrf, "system_id": e.SystemID, "key": "ui-upload"} {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
		part, err := writer.CreateFormFile("file", "note.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, strings.Repeat("x", size)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", ui.URL+"/upload", &body)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth("operator", fixtureUIToken)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("Origin", ui.URL)
		response, err := ui.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		want := 202
		if size > 3072 {
			want = 400
		}
		if response.StatusCode != want {
			t.Fatalf("upload: %d %s", response.StatusCode, data)
		}
	}
	assertCount(t, engine.store, "artifacts", 1)
	var count int
	if err := testDB(t, engine.store).QueryRow(`SELECT count(*) FROM events WHERE type='user.input'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("attachment input duplicated")
	}
	var content []byte
	if err := testDB(t, engine.store).QueryRow(`SELECT content FROM artifacts`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	var attachment map[string]string
	if err := json.Unmarshal(content, &attachment); err != nil || attachment["text"] != strings.Repeat("x", 10) {
		t.Fatalf("attachment not inert text: %s %v", content, err)
	}
}
