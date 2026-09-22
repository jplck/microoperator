//go:build linux

package ui

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/jplck/microoperator/internal/state"
)

func attribute(node *html.Node, key string) (string, bool) {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val, true
		}
	}
	return "", false
}

func walkPage(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walkPage(child, visit)
	}
}

func visibleText(node *html.Node) string {
	if node.Type == html.ElementNode {
		if node.Data == "head" || node.Data == "style" || node.Data == "script" {
			return ""
		}
		if node.Data == "details" {
			if _, open := attribute(node, "open"); !open {
				return ""
			}
		}
	}
	if node.Type == html.TextNode {
		return node.Data
	}
	var text strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(visibleText(child))
	}
	return text.String()
}

func parsedPage(t *testing.T, output string) *html.Node {
	t.Helper()
	page, err := html.Parse(strings.NewReader(output))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func renderFixture(t *testing.T, page uiPage, status int) (*html.Node, string) {
	t.Helper()
	app := &controlUI{csrf: "fixture-csrf", logger: log.New(io.Discard, "", 0)}
	response := httptest.NewRecorder()
	app.render(response, status, page)
	if response.Code != status {
		t.Fatalf("render failed: %d %s", response.Code, response.Body.String())
	}
	return parsedPage(t, response.Body.String()), response.Body.String()
}

func TestPresentationKeepsJSONInsideClosedProperties(t *testing.T) {
	table, err := collectionTable([]byte(`{"tools":[{"tool_id":"local.example","kind":"skill","state":"draft","version":1,"description":"<script>untrusted</script>"}]}`), "tools", "/tools")
	if err != nil {
		t.Fatal(err)
	}
	page, output := renderFixture(t, uiPage{Title: "Tools", Data: `{"metadata":"private details"}`, Tables: []uiTable{table},
		Forms: []uiForm{{Title: "Edit configuration", Body: `{"command":"advanced"}`}}}, 200)
	jsonFields, rows := 0, 0
	walkPage(page, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		if node.Data == "script" {
			t.Fatal("untrusted cell became executable markup")
		}
		if node.Data == "tr" {
			rows++
		}
		name, _ := attribute(node, "name")
		if node.Data != "pre" && !(node.Data == "textarea" && name == "json") {
			return
		}
		jsonFields++
		hidden := false
		for ancestor := node.Parent; ancestor != nil; ancestor = ancestor.Parent {
			class, _ := attribute(ancestor, "class")
			_, open := attribute(ancestor, "open")
			if ancestor.Data == "details" && class == "properties" && !open {
				hidden = true
			}
		}
		if !hidden {
			t.Fatal("JSON visible outside a closed Properties menu")
		}
	})
	if jsonFields != 2 || rows != 2 || !strings.Contains(visibleText(page), "local.example") ||
		strings.Contains(visibleText(page), "private details") || strings.Contains(visibleText(page), `"command"`) {
		t.Fatalf("table or property disclosure is incorrect: %s", output)
	}
}

func TestPendingGoalStartNeedsNoReentryAndErrorsStayVisible(t *testing.T) {
	for _, pending := range []string{"A saved goal.", ""} {
		page, _ := renderFixture(t, uiPage{Title: "System", Forms: []uiForm{
			{Title: "Start system", Kind: "start", PendingGoal: pending, Revision: 3, Path: "/v1/systems/fixture/start"},
		}}, 200)
		goals := 0
		continuous := 0
		walkPage(page, func(node *html.Node) {
			name, _ := attribute(node, "name")
			if node.Data == "input" && name == "continuous" {
				continuous++
				_, checked := attribute(node, "checked")
				value, _ := attribute(node, "value")
				if !checked || value != "true" {
					t.Fatal("start did not offer an explicit continuous default")
				}
			}
			if node.Data == "textarea" && name == "goal" {
				goals++
				_, required := attribute(node, "required")
				if required != (pending == "") || visibleText(node) != "" {
					t.Fatal("pending goal was re-sent as a new goal or required reentry")
				}
			}
		})
		if goals != 1 || continuous != 1 || !strings.Contains(visibleText(page), "Start system") {
			t.Fatal("missing start action")
		}
	}
	page, _ := renderFixture(t, uiPage{Title: "Command rejected", Data: `{"error":"token budget exhausted"}`}, 400)
	if !strings.Contains(visibleText(page), "token budget exhausted") || strings.Contains(visibleText(page), `"error"`) {
		t.Fatal("hiding JSON also hid a command failure")
	}
}

func TestContinuousSystemSummaryShowsIdleWithoutEndingGoal(t *testing.T) {
	record := state.SystemRecord{ID: "fixture", State: "running", Continuous: true, Idle: true,
		Execution: &state.ExecutionRecord{Prompt: "Keep helping", Response: "Ready"}}
	var page uiPage
	systemSummary(&page, record)
	if page.State != "waiting" || page.GoalLabel != "Continuous goal" || page.Outcome != "Ready" ||
		!strings.Contains(page.Notice, "idle") {
		t.Fatalf("continuous idle presentation: %+v", page)
	}
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestActivityPresentationShowsRecordedWorkAndCanPauseRefresh(t *testing.T) {
	id := "sys_0123456789abcdef0123456789abcdef"
	snapshot := state.ActivitySnapshot{
		System: state.SystemRecord{ID: id, OperatorID: "operator", State: "running",
			Configuration: state.SystemConfig{Name: "Fixture team"}},
		Agents: []state.ActivityAgent{
			{ID: "operator", Name: "operator", State: "active", TaskState: "waiting", TaskID: "parent-task", Reason: "Waiting for delegated analysis."},
			{ID: "child", Parent: "operator", Depth: 1, Name: "researcher", State: "active", TaskState: "running", TaskID: "child-task", Assignment: "Check the evidence.", Model: "local"},
		},
		Calls:  []state.ActivityCall{{AgentID: "child", TaskID: "child-task", Model: "local", State: "running", CreatedAt: 1000}},
		Events: []state.ActivityEvent{{Type: "task.result", Source: "child", Recipient: "operator", State: "pending", CreatedAt: 1000}},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/systems/"+id+"/activity" {
			t.Fatalf("unexpected activity endpoint: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
	})}
	app := &controlUI{client: client, logger: log.New(io.Discard, "", 0)}
	for _, query := range []string{"", "?refresh=off"} {
		request := httptest.NewRequest("GET", "/systems/"+id+"/activity"+query, nil)
		request.SetPathValue("system_id", id)
		response := httptest.NewRecorder()
		app.activity(response, request)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		page := parsedPage(t, response.Body.String())
		text := visibleText(page)
		for _, wanted := range []string{"Operator", "researcher", "Created by operator", "Calling model local", "Waiting for delegated analysis.", "Task result"} {
			if !strings.Contains(text, wanted) {
				t.Fatalf("missing %q in activity: %s", wanted, text)
			}
		}
		if strings.Contains(response.Body.String(), `http-equiv="refresh"`) != (query == "") {
			t.Fatal("activity refresh cannot be paused")
		}
		if strings.Contains(response.Body.String(), "#ZgotmplZ") || strings.Contains(text, `"agent_id"`) ||
			strings.Contains(response.Body.String(), `<form`) {
			t.Fatal("activity exposed JSON, invalid styles, or auto-refreshing editable forms")
		}
	}
}

func TestCollectionTablesRejectMalformedShapesAndPreserveNumbers(t *testing.T) {
	for _, data := range []string{`{}`, `{"calls":{}}`, `{"calls":[null]}`} {
		if _, err := collectionTable([]byte(data), "model-calls", ""); err == nil {
			t.Fatalf("malformed collection accepted: %s", data)
		}
	}
	table, err := collectionTable([]byte(`{"calls":[{"model":"fixture","input_tokens":9007199254740993,"usage_known":true,"state":"completed"}]}`), "model-calls", "")
	if err != nil || table.Rows[0][3].Text != "9007199254740993" {
		t.Fatalf("usage number lost precision: %+v %v", table, err)
	}
	if preview(`{"secret":"metadata"}`) != "Structured content - open Properties" {
		t.Fatal("structured content escaped Properties")
	}
}
