//go:build linux

package ui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/state"
)

const TokenEnv = "MICROOPERATOR_UI_TOKEN"

const maxUIView = 2 << 20

func NewClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func Run(ctx context.Context, socket, address string, stdout, stderr io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("UI requires a numeric loopback listen address")
	}
	controlToken, uiToken := os.Getenv(protocol.ControlTokenEnv), os.Getenv(TokenEnv)
	if !protocol.ValidAPIToken(controlToken) || !protocol.ValidAPIToken(uiToken) || controlToken == uiToken {
		return errors.New("UI requires distinct 32-256 character MICROOPERATOR_CONTROL_TOKEN and MICROOPERATOR_UI_TOKEN values")
	}
	socket, err = filepath.Abs(socket)
	if err != nil {
		return err
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("UI daemon socket must be a Unix socket, not a symlink")
	}
	client := NewClient(socket)
	defer client.CloseIdleConnections()
	_, status, err := uiRequest(ctx, client, controlToken, "GET", "/v1/health", nil, "")
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("daemon readiness/authentication returned HTTP %d", status)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	logger := log.New(stderr, "ui: ", log.LstdFlags)
	handler := NewHandler(client, controlToken, uiToken, listener.Addr().String(), logger)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: logger}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	if err := protocol.WriteMessage(stdout, protocol.Message{Type: "ready", Data: "http://" + listener.Addr().String()}); err != nil {
		server.Close()
		<-finished
		return err
	}
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			err = errors.Join(err, server.Close())
		}
		serveErr := <-finished
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}

func uiRequest(ctx context.Context, client *http.Client, token, method, target string, body []byte, key string) ([]byte, int, error) {
	parsed, err := url.ParseRequestURI(target)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/v1/") || path.Clean(parsed.Path) != parsed.Path || strings.Contains(parsed.Path, "\\") {
		return nil, 0, errors.New("invalid daemon API path")
	}
	parsed.Scheme, parsed.Host = "http", "daemon"
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("daemon request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUIView+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxUIView {
		return nil, 0, errors.New("daemon response exceeds UI limit")
	}
	return data, response.StatusCode, nil
}

type controlUI struct {
	client              *http.Client
	control, csrf, host string
	logger              *log.Logger
}

type uiLink struct{ Title, URL string }

type uiForm struct {
	Title, Kind, Path, Method, Body, Key, Return string
	Revision                                     int64
	PendingGoal                                  string
	Danger                                       bool
}

type uiPage struct {
	Title, Data, CSRF, SystemID, UploadKey, Notice   string
	Subtitle, State, Goal, GoalLabel, Outcome, Error string
	Defaults                                         state.SystemConfig
	Links                                            []uiLink
	Forms                                            []uiForm
	Tables                                           []uiTable
	Stats                                            []uiStat
	Agents                                           []uiAgent
	Events                                           []uiEvent
	Refresh                                          bool
	AllowUpload, HasProperties                       bool
}

func NewHandler(client *http.Client, controlToken, uiToken, host string, logger *log.Logger) http.Handler {
	// A stateless, origin-bound token survives UI restart without giving the
	// browser the daemon credential or storing any execution state in the UI.
	mac := hmac.New(sha256.New, []byte(uiToken))
	mac.Write([]byte("microoperator-ui-csrf-v1:" + host))
	app := &controlUI{client: client, control: controlToken, csrf: hex.EncodeToString(mac.Sum(nil)), host: host, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.home)
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) { app.view(w, r, "Shared tool catalog", "/v1/tools") })
	mux.HandleFunc("GET /broker", func(w http.ResponseWriter, r *http.Request) {
		app.view(w, r, "Model admission and usage", "/v1/model-broker")
	})
	mux.HandleFunc("GET /systems/{system_id}", app.system)
	mux.HandleFunc("GET /systems/{system_id}/activity", app.activity)
	mux.HandleFunc("GET /systems/{system_id}/{section}", app.section)
	mux.HandleFunc("GET /systems/{system_id}/artifacts/{artifact_id}", func(w http.ResponseWriter, r *http.Request) {
		if !state.SystemIDPattern.MatchString(r.PathValue("system_id")) {
			http.NotFound(w, r)
			return
		}
		app.view(w, r, "Artifact", "/v1/systems/"+r.PathValue("system_id")+"/artifacts/"+url.PathEscape(r.PathValue("artifact_id")))
	})
	mux.HandleFunc("POST /command", app.command)
	mux.HandleFunc("POST /upload", app.upload)
	expected := sha256.Sum256([]byte(uiToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// no-referrer turns native form POSTs' Origin into "null".
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		_, port, _ := net.SplitHostPort(host)
		if r.Host != host && r.Host != "localhost:"+port {
			http.Error(w, "UI host is not an allowed loopback origin", http.StatusForbidden)
			return
		}
		user, password, ok := r.BasicAuth()
		supplied := sha256.Sum256([]byte(password))
		if !ok || user != "operator" || subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Microoperator", charset="UTF-8"`)
			http.Error(w, "UI authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method == "POST" && r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "cross-origin UI command rejected", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func prettyJSON(data []byte) string {
	var formatted bytes.Buffer
	if json.Indent(&formatted, data, "", "  ") != nil {
		return string(data)
	}
	return formatted.String()
}

func (app *controlUI) render(w http.ResponseWriter, status int, page uiPage) {
	page.CSRF = app.csrf
	if status >= 400 && page.Error == "" {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(page.Data), &failure) == nil && failure.Error != "" {
			page.Error = failure.Error
		} else if !json.Valid([]byte(page.Data)) {
			page.Error = page.Data
		} else {
			page.Error = http.StatusText(status)
		}
	}
	page.HasProperties = page.Data != ""
	if page.AllowUpload {
		key, err := state.NewID("ui_")
		if err != nil {
			http.Error(w, "cannot create attachment identity", 500)
			app.logger.Printf("attachment identity: %v", err)
			return
		}
		page.UploadKey = key
	}
	for i := range page.Forms {
		if page.Forms[i].Key == "" {
			key, err := state.NewID("ui_")
			if err != nil {
				http.Error(w, "cannot create command identity", 500)
				app.logger.Printf("command identity: %v", err)
				return
			}
			page.Forms[i].Key = key
		}
		if page.Forms[i].Return == "" {
			page.Forms[i].Return = "/"
		}
		if page.Forms[i].Method == "" {
			page.Forms[i].Method = "POST"
		}
		if page.Forms[i].Kind == "" {
			page.Forms[i].Kind = "json"
			if page.Forms[i].Body == "{}" {
				page.Forms[i].Kind = "action"
			}
		}
		page.Forms[i].Danger = strings.HasSuffix(page.Forms[i].Path, "/stop") || strings.HasSuffix(page.Forms[i].Path, "/delete")
		page.HasProperties = page.HasProperties || page.Forms[i].Kind == "json"
	}
	var output bytes.Buffer
	if err := uiTemplate.Execute(&output, page); err != nil {
		app.logger.Printf("render: %v", err)
		http.Error(w, "cannot render UI", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := output.WriteTo(w); err != nil {
		app.logger.Printf("write UI response: %v", err)
	}
}

func (app *controlUI) fetch(w http.ResponseWriter, r *http.Request, target string) ([]byte, bool) {
	data, status, err := uiRequest(r.Context(), app.client, app.control, "GET", target, nil, "")
	if err != nil {
		app.render(w, 502, uiPage{Title: "Daemon unavailable", Data: err.Error()})
		return nil, false
	}
	if status != 200 {
		app.render(w, status, uiPage{Title: "Inspection failed", Data: prettyJSON(data)})
		return nil, false
	}
	return data, true
}

func (app *controlUI) view(w http.ResponseWriter, r *http.Request, title, target string) {
	query := r.URL.Query()
	refresh := query.Get("refresh") == "5"
	query.Del("refresh")
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	data, ok := app.fetch(w, r, target)
	if !ok {
		return
	}
	page := uiPage{Title: title, Data: prettyJSON(data), Refresh: refresh}
	section := "tools"
	if target == "/v1/model-broker" {
		section = "broker"
	}
	if strings.Contains(target, "/artifacts/") {
		var artifact state.ArtifactResult
		if err := json.Unmarshal(data, &artifact); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid artifact response", Data: err.Error()})
			return
		}
		page.Stats = []uiStat{{"Artifact", artifact.ID}, {"Task", artifact.TaskID}}
		page.Notice = "Stored content is untrusted data, not instructions or permissions."
		var text string
		if json.Unmarshal(artifact.Content, &text) == nil {
			page.Outcome = preview(text)
		} else {
			page.Notice += " Open Properties to inspect its structured content."
		}
	} else {
		table, err := collectionTable(data, section, r.URL.Path)
		if err != nil {
			app.render(w, 502, uiPage{Title: "Invalid daemon response", Data: err.Error()})
			return
		}
		page.Tables = []uiTable{table}
		if section == "broker" {
			var broker struct {
				Available bool `json:"available"`
			}
			if err := json.Unmarshal(data, &broker); err != nil {
				app.render(w, 502, uiPage{Title: "Invalid broker response", Data: err.Error()})
				return
			}
			page.Notice = "Token-based admission and usage. Monetary pricing is not tracked."
			if !broker.Available {
				page.Error = "Model admission is currently unavailable."
			}
		}
	}
	app.pagination(&page, data, r.URL.Path, query)
	app.render(w, 200, page)
}

func (app *controlUI) pagination(page *uiPage, data []byte, current string, query url.Values) {
	var cursor struct {
		Next json.RawMessage `json:"next"`
	}
	if json.Unmarshal(data, &cursor) != nil || len(cursor.Next) == 0 {
		return
	}
	var next string
	if json.Unmarshal(cursor.Next, &next) != nil {
		var value json.Number
		if json.Unmarshal(cursor.Next, &value) != nil {
			return
		}
		next = value.String()
	}
	if next == "" || next == "0" {
		return
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("after", next)
	page.Links = append(page.Links, uiLink{"Next page", current + "?" + query.Encode()})
}

func (app *controlUI) home(w http.ResponseWriter, r *http.Request) {
	target := "/v1/systems"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	data, ok := app.fetch(w, r, target)
	if !ok {
		return
	}
	var records struct {
		Systems []state.SystemRecord `json:"systems"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		app.render(w, 502, uiPage{Title: "Invalid daemon response", Data: err.Error()})
		return
	}
	defaultsData, ok := app.fetch(w, r, "/v1/system-defaults")
	if !ok {
		return
	}
	var defaults state.SystemConfig
	if err := json.Unmarshal(defaultsData, &defaults); err != nil || defaults.Operator.Model == "" || defaults.Limits.TokenBudget <= 0 {
		app.render(w, 502, uiPage{Title: "Invalid daemon response", Data: "Invalid bootstrap defaults"})
		return
	}
	page := uiPage{Title: "Systems", Subtitle: "Give each system a goal. Start work when you are ready.", Data: prettyJSON(data), Defaults: defaults,
		Forms: []uiForm{{Title: "Create system", Kind: "create", Path: "/v1/systems"}}}
	table := uiTable{Title: "Your systems", Columns: []string{"System", "State", "Goal", "Model", "Tokens remaining", "Activity"}}
	for _, record := range records.Systems {
		name, goal := record.Configuration.Name, ""
		if name == "" {
			name = record.ID
		}
		if record.Goal != nil {
			goal = record.Goal.Prompt
		}
		if record.Execution != nil {
			goal = record.Execution.Prompt
		}
		table.Rows = append(table.Rows, []uiCell{
			{Text: name, URL: "/systems/" + record.ID}, {Text: record.State, Status: true}, {Text: preview(goal)},
			{Text: record.Configuration.Operator.Model}, {Text: strconv.FormatInt(record.RemainingTokens, 10)},
			{Text: "Watch activity", URL: "/systems/" + record.ID + "/activity"},
		})
	}
	page.Tables = []uiTable{table}
	app.pagination(&page, data, "/", r.URL.Query())
	app.render(w, 200, page)
}

func (app *controlUI) system(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if !state.SystemIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	base := "/v1/systems/" + id
	data, ok := app.fetch(w, r, base)
	if !ok {
		return
	}
	var record state.SystemRecord
	if err := json.Unmarshal(data, &record); err != nil {
		app.render(w, 502, uiPage{Title: "Invalid system response", Data: err.Error()})
		return
	}
	page := uiPage{Title: record.Configuration.Name, Subtitle: "System overview", SystemID: id, Data: prettyJSON(data)}
	if page.Title == "" {
		page.Title = id
	}
	systemSummary(&page, record)
	page.Links = append(page.Links, uiLink{"Watch activity", "/systems/" + id + "/activity"})
	for _, section := range []string{"agents", "tasks", "events", "model-calls", "tool-calls", "artifacts", "tools", "memory", "schedules", "subscriptions", "learning", "learning-checks", "learning-feedback"} {
		page.Links = append(page.Links, uiLink{label(section), "/systems/" + id + "/" + section})
	}
	if record.State == "inactive" || record.State == "stopped" {
		if record.BlockedReason == "" && record.RemainingTokens > 0 {
			start := uiForm{Title: "Start new goal", Kind: "start", Path: base + "/start", Revision: record.Revision, Return: "/systems/" + id}
			if record.Goal != nil && record.Goal.State == "pending" {
				start.Title, start.PendingGoal = "Start system", preview(record.Goal.Prompt)
				page.Notice = "Your initial goal is saved, not running. Click Start system below to run it; you do not need to enter it again."
			}
			page.Forms = append(page.Forms, start)
		} else if record.RemainingTokens <= 0 {
			page.Error = "Token budget exhausted. Revise the budget in Properties before starting another goal."
		}
	}
	if record.State == "running" || record.State == "paused" {
		page.AllowUpload = true
		page.Forms = append(page.Forms, uiForm{Title: "Send input", Kind: "input", Path: base + "/input", Return: "/systems/" + id})
		action := "pause"
		if record.State == "paused" {
			action = "resume"
		}
		page.Forms = append(page.Forms,
			uiForm{Title: label(action) + " system", Path: base + "/" + action, Body: "{}", Return: "/systems/" + id},
			uiForm{Title: "Stop system", Path: base + "/stop", Body: "{}", Return: "/systems/" + id})
	}
	body, err := json.MarshalIndent(state.ReviseSystemCommand{ExpectedRevision: record.Revision, Configuration: &record.Configuration}, "", "  ")
	if err != nil {
		app.render(w, 500, uiPage{Title: "Cannot render configuration", Data: err.Error()})
		return
	}
	page.Forms = append(page.Forms, uiForm{Title: "Revise future configuration/grants/budget (stop first)", Path: base + "/configuration", Method: "PUT", Body: string(body), Return: "/systems/" + id})
	app.render(w, 200, page)
}

func (app *controlUI) section(w http.ResponseWriter, r *http.Request) {
	id, section := r.PathValue("system_id"), r.PathValue("section")
	if !state.SystemIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if !slices.Contains([]string{"agents", "tasks", "events", "model-calls", "tool-calls", "artifacts", "tools", "memory", "schedules", "subscriptions", "learning", "learning-checks", "learning-feedback"}, section) {
		http.NotFound(w, r)
		return
	}
	base := "/v1/systems/" + id
	query := r.URL.Query()
	refresh := query.Get("refresh") == "5"
	query.Del("refresh")
	target := base + "/" + section
	if strings.HasPrefix(section, "learning-") {
		target = base + "/learning/" + strings.TrimPrefix(section, "learning-")
	}
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	data, ok := app.fetch(w, r, target)
	if !ok {
		return
	}
	back := "/systems/" + id + "/" + section
	page := uiPage{Title: label(section), Subtitle: id, Data: prettyJSON(data), SystemID: id, Refresh: refresh,
		Links: []uiLink{{"System overview", "/systems/" + id}, {"Watch activity", "/systems/" + id + "/activity"}}}
	table, err := collectionTable(data, section, back)
	if err != nil {
		app.render(w, 502, uiPage{Title: "Invalid daemon response", Data: err.Error()})
		return
	}
	page.Tables = []uiTable{table}
	app.pagination(&page, data, back, query)
	switch section {
	case "learning":
		page.Forms = append(page.Forms, uiForm{Title: "Evaluate an exact proposal against protected checks", Path: base + "/learning/evaluate", Return: back, Body: `{"tool_id":"","version":1,"check_id":"","task_id":""}`})
		var evaluations struct {
			Items []struct {
				ID     string `json:"evaluation_id"`
				State  string `json:"state"`
				Digest string `json:"digest"`
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &evaluations); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid evaluation response", Data: err.Error()})
			return
		}
		for _, evaluation := range evaluations.Items {
			if evaluation.State != "pending_approval" {
				continue
			}
			body, err := json.MarshalIndent(map[string]any{"digest": evaluation.Digest, "expires_seconds": 3600, "task_uses": 1}, "", "  ")
			if err != nil {
				app.render(w, 500, uiPage{Title: "Cannot render approval", Data: err.Error()})
				return
			}
			page.Forms = append(page.Forms, uiForm{Title: "Approve exact evidence " + evaluation.ID + " (does not assign)", Path: base + "/learning/" + evaluation.ID + "/approve", Return: back, Body: string(body)})
		}
	case "learning-checks":
		page.Forms = append(page.Forms, uiForm{Title: "Create immutable protected acceptance checks", Path: base + "/learning/checks", Return: back, Body: `{"cases":[{"input":"example","expected":"EXAMPLE"}]}`})
	case "learning-feedback":
		page.Forms = append(page.Forms, uiForm{Title: "Record user feedback", Path: base + "/learning/feedback", Return: back, Body: `{"task_id":"","rating":"failure","content":""}`})
	case "agents":
		var records struct {
			Agents []state.AgentRecord `json:"agents"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid agent response", Data: err.Error()})
			return
		}
		for _, agent := range records.Agents {
			for _, action := range []string{"pause", "resume", "stop"} {
				page.Forms = append(page.Forms, uiForm{Title: action + " " + agent.Name + " (" + agent.ID + ")", Path: base + "/agents/" + agent.ID + "/" + action, Body: "{}", Return: back})
			}
			if agent.Parent != "" {
				body, _ := json.MarshalIndent(map[string]any{"expected_revision": agent.Revision, "tools": agent.Tools}, "", "  ")
				page.Forms = append(page.Forms, uiForm{Title: "Assign future tools to " + agent.Name, Method: "PUT", Path: base + "/agents/" + agent.ID + "/tools", Body: string(body), Return: back})
			}
		}
	case "tasks":
		var records struct {
			Tasks []state.TaskRecord `json:"tasks"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid task response", Data: err.Error()})
			return
		}
		goals := map[string]bool{}
		for _, task := range records.Tasks {
			if !state.TaskTerminal(task.State) && !goals[task.GoalID] {
				goals[task.GoalID] = true
				for _, action := range []string{"pause", "resume", "stop"} {
					page.Forms = append(page.Forms, uiForm{Title: action + " goal " + task.GoalID, Path: base + "/goals/" + task.GoalID + "/" + action, Body: "{}", Return: back})
				}
			}
		}
	case "tools":
		page.Forms = append(page.Forms, uiForm{Title: "Submit an inert local draft", Path: base + "/tools/drafts", Body: `{"kind":"skill","description":"","content":"","requires_tools":[]}`, Return: back},
			uiForm{Title: "Revoke a shared version", Path: base + "/tools/revoke", Body: `{"name":"","version":1}`, Return: back})
		var records struct {
			Tools []state.RegistryEntry `json:"tools"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid catalog response", Data: err.Error()})
			return
		}
		for _, tool := range records.Tools {
			if tool.SystemID == id {
				for _, state := range []string{"rejected", "disabled"} {
					body, _ := json.Marshal(map[string]any{"version": tool.Version, "state": state})
					page.Forms = append(page.Forms, uiForm{Title: state + " " + tool.ID, Path: base + "/tools/" + tool.ID + "/state", Body: string(body), Return: back})
				}
			}
		}
	case "memory":
		page.Links = append(page.Links, uiLink{"Pending shared facts", back + "?scope=system&include_pending=true"}, uiLink{"Operator memory", back + "?scope=agent"})
		page.Forms = append(page.Forms, uiForm{Title: "Write a reviewed note", Path: base + "/memory", Body: `{"scope":"system","content":"","evidence":"","confidence":50,"artifacts":[],"retention_seconds":86400}`, Return: back})
		var records struct {
			Entries []state.MemoryEntry `json:"entries"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid memory response", Data: err.Error()})
			return
		}
		for _, entry := range records.Entries {
			for _, action := range []string{"approve", "delete"} {
				if action == "approve" && entry.State != "pending" {
					continue
				}
				body, _ := json.Marshal(map[string]int64{"expected_revision": entry.Revision})
				page.Forms = append(page.Forms, uiForm{Title: action + " " + entry.ID, Path: base + "/memory/" + entry.ID + "/" + action, Body: string(body), Return: back})
			}
		}
	case "schedules", "subscriptions":
		if section == "schedules" {
			page.Forms = append(page.Forms, uiForm{Title: "Create a bounded schedule", Path: base + "/schedules", Body: `{"cron":"* * * * *","timezone":"UTC","content":"","trigger_budget":1}`, Return: back})
		} else {
			page.Forms = append(page.Forms, uiForm{Title: "Subscribe the active task", Path: base + "/subscriptions", Body: `{"type":"memory.changed","scope":"system","trigger_budget":1}`, Return: back})
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid trigger response", Data: err.Error()})
			return
		}
		var entries []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(envelope[section], &entries); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid trigger entries", Data: err.Error()})
			return
		}
		for _, entry := range entries {
			page.Forms = append(page.Forms, uiForm{Title: "Cancel " + entry.ID, Path: base + "/" + section + "/" + entry.ID + "/cancel", Body: "{}", Return: back})
		}
	}
	if len(page.Forms) > 0 {
		page.Refresh = false
	}
	app.render(w, 200, page)
}

func (app *controlUI) checkForm(w http.ResponseWriter, r *http.Request, multipart bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 3*protocol.MaxFrame+8192)
	var err error
	if multipart {
		err = r.ParseMultipartForm(3*protocol.MaxFrame + 8192)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		http.Error(w, "invalid or oversized UI form", 400)
		return false
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			http.Error(w, "repeated form field", 400)
			return false
		}
	}
	expected := sha256.Sum256([]byte(app.csrf))
	supplied := sha256.Sum256([]byte(r.PostForm.Get("csrf")))
	if subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
		http.Error(w, "invalid CSRF token", 403)
		return false
	}
	return true
}

func (app *controlUI) command(w http.ResponseWriter, r *http.Request) {
	if !app.checkForm(w, r, false) {
		return
	}
	form := r.PostForm
	method, target, key := form.Get("method"), form.Get("path"), form.Get("key")
	if method != "POST" && method != "PUT" {
		http.Error(w, "invalid command method", 400)
		return
	}
	var value any
	switch form.Get("kind") {
	case "create":
		budget := int64(0)
		if raw := form.Get("token_budget"); raw != "" {
			var err error
			budget, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				http.Error(w, "invalid token_budget", 400)
				return
			}
		}
		goal := form.Get("goal")
		value = state.CreateSystemCommand{Name: form.Get("name"), Goal: &goal, Constraints: form.Get("constraints"), TokenBudget: budget}
	case "start":
		var revision, budget, lifetime int64
		for field, out := range map[string]*int64{"expected_revision": &revision, "token_budget": &budget, "lifetime_seconds": &lifetime} {
			raw := form.Get(field)
			if raw == "" {
				raw = "0"
			}
			number, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				http.Error(w, "invalid "+field, 400)
				return
			}
			*out = number
		}
		command := state.StartSystemCommand{ExpectedRevision: revision, TokenBudget: budget, LifetimeSeconds: lifetime}
		switch form.Get("continuous") {
		case "true":
			command.Continuous = true
		case "":
		default:
			http.Error(w, "invalid continuous mode", 400)
			return
		}
		if goal := form.Get("goal"); goal != "" {
			command.Goal = &goal
		}
		value = command
	case "input":
		value = map[string]string{"content": form.Get("content"), "agent_id": form.Get("agent_id")}
	case "json", "action":
		raw := []byte(form.Get("json"))
		if len(raw) > protocol.MaxFrame || protocol.DecodeJSON(raw, &value) != nil {
			http.Error(w, "invalid JSON command", 400)
			return
		}
	default:
		http.Error(w, "unknown form operation", 400)
		return
	}
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "cannot encode command", 400)
		return
	}
	app.submit(w, r, method, target, key, body, form.Get("return"))
}

func (app *controlUI) submit(w http.ResponseWriter, r *http.Request, method, target, key string, body []byte, back string) {
	if len(body) > protocol.MaxFrame || !state.CommandKeyPattern.MatchString(key) {
		http.Error(w, "invalid command size or identity", 400)
		return
	}
	if back == "" {
		back = "/"
	}
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.Contains(back, "\\") || path.Clean(back) != back {
		http.Error(w, "invalid return path", 400)
		return
	}
	data, status, err := uiRequest(r.Context(), app.client, app.control, method, target, body, key)
	page := uiPage{Title: "Command accepted", Notice: "The daemon recorded your command. Open the system to see its current state.",
		Data: prettyJSON(data), Links: []uiLink{{"Continue", back}}}
	if err != nil {
		status = 502
		page.Title, page.Data = "Daemon request failed", err.Error()
		page.Notice = "The outcome is unknown. Retrying uses the same command identity to avoid duplicate work."
		page.Forms = []uiForm{{Title: "Retry the same durable command", Kind: "action", Path: target, Method: method, Body: string(body), Key: key, Return: back}}
	} else if status >= 300 {
		page.Title = "Command rejected"
		page.Notice = ""
	}
	if status >= 200 && status < 300 {
		var record state.SystemRecord
		if json.Unmarshal(data, &record) == nil && state.SystemIDPattern.MatchString(record.ID) {
			systemSummary(&page, record)
			page.Links = append(page.Links, uiLink{"Open system", "/systems/" + record.ID}, uiLink{"Watch activity", "/systems/" + record.ID + "/activity"})
			if target == "/v1/systems" {
				page.Title = "System created"
				page.Notice = "Your initial goal is saved. Open the system and click Start system to begin; creation does not make model calls."
			} else if strings.HasSuffix(target, "/start") {
				page.Title = "Start accepted"
				page.Notice = "Work is queued for the daemon. Watch activity to follow the operator and agents."
			}
		}
	}
	app.render(w, status, page)
}

func (app *controlUI) upload(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				app.logger.Printf("remove upload temporary data: %v", err)
			}
		}
	}()
	if !app.checkForm(w, r, true) {
		return
	}
	id := r.PostForm.Get("system_id")
	if !state.SystemIDPattern.MatchString(id) {
		http.Error(w, "invalid system", 400)
		return
	}
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		http.Error(w, "one text attachment is required", 400)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing attachment", 400)
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 3073))
	err = errors.Join(err, file.Close())
	if err != nil || len(data) == 0 || len(data) > 3072 || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		http.Error(w, "attachment requires 1-3072 bytes of UTF-8 text", 400)
		return
	}
	body, err := json.Marshal(state.AttachmentCommand{AgentID: r.PostForm.Get("agent_id"), Name: header.Filename, Content: string(data)})
	if err != nil {
		http.Error(w, "cannot encode attachment", 400)
		return
	}
	app.submit(w, r, "POST", "/v1/systems/"+id+"/attachments", r.PostForm.Get("key"), body, "/systems/"+id)
}

//go:embed page.html
var pageHTML string

var uiTemplate = template.Must(template.New("ui").Parse(pageHTML))
