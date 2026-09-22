//go:build linux

package ui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
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
}

type uiPage struct {
	Title, Data, CSRF, SystemID, UploadKey string
	Links                                  []uiLink
	Forms                                  []uiForm
	Refresh                                bool
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
	mux.HandleFunc("GET /launches", func(w http.ResponseWriter, r *http.Request) {
		app.view(w, r, "Launch configurations", "/v1/launch-configurations")
	})
	mux.HandleFunc("GET /broker", func(w http.ResponseWriter, r *http.Request) {
		app.view(w, r, "Model admission and usage", "/v1/model-broker")
	})
	mux.HandleFunc("GET /systems/{system_id}", app.system)
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
		w.Header().Set("Referrer-Policy", "no-referrer")
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
	if page.SystemID != "" {
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
		}
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
	page := uiPage{Title: "Systems", Data: prettyJSON(data), Forms: []uiForm{{Title: "Create an inactive system", Kind: "create", Path: "/v1/systems"}}}
	for _, record := range records.Systems {
		page.Links = append(page.Links, uiLink{record.Launch + " / " + record.State + " / " + record.ID, "/systems/" + record.ID})
	}
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
	page := uiPage{Title: record.Launch + " / " + record.State, SystemID: id, Data: prettyJSON(data)}
	for _, section := range []string{"agents", "tasks", "events", "model-calls", "tool-calls", "artifacts", "tools", "memory", "schedules", "subscriptions", "learning", "learning-checks", "learning-feedback"} {
		page.Links = append(page.Links, uiLink{section, "/systems/" + id + "/" + section})
	}
	page.Forms = []uiForm{{Title: "Start a goal", Kind: "start", Path: base + "/start", Revision: record.Revision, Return: "/systems/" + id},
		{Title: "Send durable input", Kind: "input", Path: base + "/input", Return: "/systems/" + id}}
	for _, action := range []string{"pause", "resume", "stop"} {
		page.Forms = append(page.Forms, uiForm{Title: action + " system", Path: base + "/" + action, Body: "{}", Return: "/systems/" + id})
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
	allowed := false
	for _, name := range []string{"agents", "tasks", "events", "model-calls", "tool-calls", "artifacts", "tools", "memory", "schedules", "subscriptions", "learning", "learning-checks", "learning-feedback"} {
		if section == name {
			allowed = true
		}
	}
	if !allowed {
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
	page := uiPage{Title: section + " / " + id, Data: prettyJSON(data), SystemID: id, Refresh: refresh, Links: []uiLink{{"System", "/systems/" + id}}}
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
	case "artifacts":
		var records struct {
			Artifacts []struct {
				ID string `json:"artifact_id"`
			} `json:"artifacts"`
		}
		if err := json.Unmarshal(data, &records); err != nil {
			app.render(w, 502, uiPage{Title: "Invalid artifacts response", Data: err.Error()})
			return
		}
		for _, entry := range records.Artifacts {
			page.Links = append(page.Links, uiLink{entry.ID, back + "/" + entry.ID})
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
		value = map[string]string{"launch": form.Get("launch"), "goal": form.Get("goal")}
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
		if goal := form.Get("goal"); goal != "" {
			command.Goal = &goal
		}
		value = command
	case "input":
		value = map[string]string{"content": form.Get("content"), "agent_id": form.Get("agent_id")}
	case "json":
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
	page := uiPage{Title: "Command accepted", Data: prettyJSON(data), Links: []uiLink{{"Return to inspection", back}}}
	if err != nil {
		status = 502
		page.Title, page.Data = "Daemon request failed", err.Error()
		page.Forms = []uiForm{{Title: "Retry the same durable command", Path: target, Method: method, Body: string(body), Key: key, Return: back}}
	} else if status >= 300 {
		page.Title = "Command rejected"
	}
	if status >= 200 && status < 300 && target == "/v1/systems" {
		var record state.SystemRecord
		if json.Unmarshal(data, &record) == nil && state.SystemIDPattern.MatchString(record.ID) {
			page.Links = append(page.Links, uiLink{"Inspect created system", "/systems/" + record.ID})
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

var uiTemplate = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Microoperator - {{.Title}}</title>{{if .Refresh}}<meta http-equiv="refresh" content="5">{{end}}
<style>body{font:16px system-ui;max-width:1100px;margin:2rem auto;padding:0 1rem;color:#20252b;background:#fafbfc}nav,a{margin-right:1rem}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#eef1f4;padding:1rem}form{border:1px solid #ccd2da;padding:1rem;margin:1rem 0;background:white}label{display:block;margin:.7rem 0}textarea{display:block;width:98%;min-height:6rem;font:14px monospace}input{padding:.4rem;max-width:95%}button{padding:.5rem 1rem;cursor:pointer}small{display:block;color:#555}h2{font-size:1.15rem}</style></head>
<body><nav><a href="/">Systems</a><a href="/launches">Launch configurations</a><a href="/tools">Tool catalog</a><a href="/broker">Model broker</a></nav>
<h1>{{.Title}}</h1><p>This UI is a separate client. Closing it does not stop the daemon or approve proposals.</p>
{{range .Links}}<p><a href="{{.URL}}">{{.Title}}</a></p>{{end}}
{{if .Data}}<pre>{{.Data}}</pre>{{end}}
{{range .Forms}}<form method="post" action="/command"><h2>{{.Title}}</h2>
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="key" value="{{.Key}}">
<input type="hidden" name="kind" value="{{.Kind}}"><input type="hidden" name="path" value="{{.Path}}">
<input type="hidden" name="method" value="{{.Method}}"><input type="hidden" name="return" value="{{.Return}}">
{{if eq .Kind "create"}}<label>Launch configuration <input name="launch" required placeholder="research"></label><label>Initial goal (creation does not run it)<textarea name="goal" maxlength="32768"></textarea></label>
{{else if eq .Kind "start"}}<input type="hidden" name="expected_revision" value="{{.Revision}}"><label>New goal (leave blank only for the pending initial goal)<textarea name="goal" maxlength="32768"></textarea></label><label>Token cap (0 = remaining allowance)<input type="number" min="0" name="token_budget" value="0"></label><label>Goal lifetime in seconds (0 = default; maximum 86400)<input type="number" min="0" max="86400" name="lifetime_seconds" value="0"></label><small>Starting a goal authorizes provider calls and consumption of its budget.</small>
{{else if eq .Kind "input"}}<label>Agent ID (blank = operator)<input name="agent_id"></label><label>Context, correction or reply<textarea name="content" maxlength="4096" required></textarea></label>
{{else}}<label>Command JSON<textarea name="json" maxlength="65536" required>{{.Body}}</textarea></label>{{end}}
<button type="submit">{{.Title}}</button><small>Command ID: {{.Key}}. Resubmitting this form reuses its durable receipt.</small></form>{{end}}
{{if .SystemID}}<form method="post" action="/upload" enctype="multipart/form-data"><h2>Attach text as untrusted context</h2>
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="system_id" value="{{.SystemID}}">
<input type="hidden" name="key" value="{{.UploadKey}}">
<label>Agent ID (blank = operator)<input name="agent_id"></label><label>UTF-8 text, at most 3072 bytes <input type="file" name="file" accept="text/plain,.txt,.md" required></label><button type="submit">Attach text</button>
<small>The daemon stores an inert scoped artifact and a durable input event. No access to the original host path is granted. Command ID: {{.UploadKey}}.</small></form>{{end}}
</body></html>`))
