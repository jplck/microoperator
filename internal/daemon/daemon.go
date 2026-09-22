//go:build linux

package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jplck/microoperator/internal/protocol"
	"github.com/jplck/microoperator/internal/sandbox"
	"github.com/jplck/microoperator/internal/state"
	nono "github.com/nolabs-ai/nono-go"
)

func lockStateDirectory(directory string) (*os.File, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create data_dir: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect data_dir: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("data_dir must be a private, user-owned directory, not a symlink")
	}
	lock, err := state.OpenOwnedFile(filepath.Join(directory, "daemon.lock"), os.O_CREATE|os.O_RDWR, true)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("lock data_dir (another daemon may be running): %w", err), lock.Close())
	}
	return lock, nil
}

func listenControl(directory string) (*net.UnixListener, error) {
	filename := filepath.Join(directory, "control.sock")
	info, err := os.Lstat(filename)
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || stat.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("control.sock exists but is not an owned socket")
		}
		connection, dialErr := net.DialTimeout("unix", filename, time.Second)
		if dialErr == nil {
			return nil, errors.Join(errors.New("control.sock is already accepting connections"), connection.Close())
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot establish that control.sock is stale: %w", dialErr)
		}
		// The exclusive directory lock is already held. Only the stale socket
		// name is removed after a crash, never the database or its lock file.
		if err := os.Remove(filename); err != nil {
			return nil, fmt.Errorf("remove stale control socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect control socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filename, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(filename, 0600); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict control socket: %w", err), listener.Close())
	}
	return listener, nil
}

// runDaemon owns the store, socket, and HTTP server until shutdown. Loading named
// launch configurations only records administrative metadata. Only explicit,
// authenticated start commands activate the reviewed operator.
func Run(ctx context.Context, configPath string, stdout, stderr io.Writer) (err error) {
	cfg, err := loadConfiguration(configPath, os.LookupEnv)
	if err != nil {
		return err
	}
	token := os.Getenv(protocol.ControlTokenEnv)
	if !protocol.ValidAPIToken(token) {
		return fmt.Errorf("%s must contain 32-256 printable non-space characters", protocol.ControlTokenEnv)
	}
	if !sandbox.Supported() || !nono.IsSupported() {
		return errors.New("configured worker profiles require an implemented platform and available nono support; this is not untrusted-code qualification")
	}
	lock, err := lockStateDirectory(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := prepareResources(&cfg); err != nil {
		return err
	}
	if err := prepareLearning(cfg, ctx); err != nil {
		return err
	}
	if err := sandbox.CleanupWorkspaces(cfg.DataDir); err != nil {
		return err
	}
	store, err := state.Open(ctx, filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	configID, err := store.RememberConfiguration(ctx, cfg)
	if err != nil {
		return err
	}
	listener, err := listenControl(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}()
	logger := log.New(stderr, "daemon: ", log.LstdFlags)
	engine, err := newExecutionEngine(ctx, store, cfg, logger)
	if err != nil {
		return err
	}
	defer engine.close()
	server := &http.Server{
		Handler:           newControlHandler(store, cfg, configID, token, logger, engine),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 8192, ErrorLog: logger,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	if err := protocol.WriteMessage(stdout, protocol.Message{Type: "ready", Data: listener.Addr().String()}); err != nil {
		closeErr := server.Close()
		serveErr := <-finished
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(fmt.Errorf("announce daemon readiness: %w", err), closeErr, serveErr)
	}
	select {
	case err := <-finished:
		return fmt.Errorf("serve control API: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		serveErr := <-finished
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}

type controlAPI struct {
	store    *state.Store
	cfg      state.Configuration
	configID string
	logger   *log.Logger
	engine   *executionEngine
}

func newControlHandler(store *state.Store, cfg state.Configuration, configID, token string, logger *log.Logger, engine *executionEngine) http.Handler {
	api := &controlAPI{store: store, cfg: cfg, configID: configID, logger: logger, engine: engine}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		available := engine != nil && engine.broker.available()
		status := "ready"
		if engine != nil && !available {
			status = "degraded"
		}
		profiles := []string{}
		for name, profile := range cfg.SandboxProfiles {
			if state.GeneratedProfile(profile) == nil {
				profiles = append(profiles, name)
			}
		}
		sort.Strings(profiles)
		api.respond(w, http.StatusOK, struct {
			Status                    string   `json:"status"`
			ExecutionEnabled          bool     `json:"execution_enabled"`
			UntrustedExecutionEnabled bool     `json:"untrusted_execution_enabled"`
			GeneratedBuildEnabled     bool     `json:"generated_build_enabled"`
			GeneratedProfiles         []string `json:"generated_profiles"`
		}{status, available, available && len(profiles) > 0, available && len(profiles) > 0 && cfg.Learning != nil, profiles})
	})
	mux.HandleFunc("GET /v1/launch-configurations", func(w http.ResponseWriter, r *http.Request) {
		type launch struct {
			Name          string             `json:"name"`
			Configuration state.SystemConfig `json:"configuration"`
		}
		names := make([]string, 0, len(cfg.Systems))
		for name := range cfg.Systems {
			names = append(names, name)
		}
		sort.Strings(names)
		launches := make([]launch, 0, len(names))
		for _, name := range names {
			launches = append(launches, launch{name, cfg.Systems[name]})
		}
		api.respond(w, http.StatusOK, struct {
			Launches []launch `json:"launches"`
		}{launches})
	})
	mux.HandleFunc("GET /v1/systems", api.list)
	mux.HandleFunc("POST /v1/systems", api.create)
	mux.HandleFunc("GET /v1/systems/{system_id}", api.get)
	mux.HandleFunc("PUT /v1/systems/{system_id}/configuration", api.revise)
	mux.HandleFunc("POST /v1/systems/{system_id}/start", api.start)
	mux.HandleFunc("POST /v1/systems/{system_id}/stop", api.stop)
	mux.HandleFunc("GET /v1/model-broker", api.brokerStatus)
	mux.HandleFunc("GET /v1/systems/{system_id}/model-calls", api.calls)
	api.registerRuntimeRoutes(mux)
	api.registerKnowledgeRoutes(mux)
	api.registerLearningRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		api.failure(w, state.ErrSystemNotFound)
	})
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		supplied := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(expected[:], supplied[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			api.respond(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		// Control requests authenticate the local administrator. Worker sessions
		// use their daemon-owned pipes, never this token or caller-supplied IDs.
		// Identity comes from this successful token check, never a request field.
		// Browser clients belong to the later UI; reject their Origin requests.
		if r.Header.Get("Origin") != "" {
			api.respond(w, http.StatusForbidden, map[string]string{"error": "browser-origin requests are not supported"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

const localAdministrator = "local-admin"

func (api *controlAPI) respond(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		api.logger.Printf("encode control response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(data, '\n')); err != nil {
		api.logger.Printf("write control response: %v", err)
	}
}

func (api *controlAPI) failure(w http.ResponseWriter, err error) {
	var field *state.FieldError
	status, message := http.StatusInternalServerError, "internal storage error"
	switch {
	case errors.As(err, &field):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, state.ErrSystemNotFound):
		status, message = http.StatusNotFound, state.ErrSystemNotFound.Error()
	case errors.Is(err, state.ErrCommandConflict), errors.Is(err, state.ErrRevisionConflict), errors.Is(err, state.ErrExecutionConflict):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, errBrokerUnavailable):
		status, message = http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusRequestTimeout, "request canceled or deadline exceeded"
	default:
		api.logger.Printf("control operation failed: %v", err)
	}
	api.respond(w, status, map[string]string{"error": message})
}

func (api *controlAPI) executionAvailable(w http.ResponseWriter) bool {
	if api.engine == nil {
		api.respond(w, http.StatusServiceUnavailable, map[string]string{"error": "execution owner unavailable"})
		return false
	}
	return true
}

func (api *controlAPI) start(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command state.StartSystemCommand
	key, err := readCommand(w, r, &command)
	id := r.PathValue("system_id")
	if err == nil && !state.SystemIDPattern.MatchString(id) {
		err = state.ErrSystemNotFound
	}
	if err == nil && command.ExpectedRevision < 1 {
		err = state.Invalid("expected_revision", "must be positive")
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.engine.start(r.Context(), localAdministrator, key, id, command)
	api.systemResponse(w, http.StatusAccepted, record, err)
}

func (api *controlAPI) stop(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	var command struct{}
	key, err := readCommand(w, r, &command)
	id := r.PathValue("system_id")
	if err == nil && !state.SystemIDPattern.MatchString(id) {
		err = state.ErrSystemNotFound
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.engine.stop(r.Context(), localAdministrator, key, id)
	api.systemResponse(w, http.StatusAccepted, record, err)
}

func (api *controlAPI) brokerStatus(w http.ResponseWriter, r *http.Request) {
	if !api.executionAvailable(w) {
		return
	}
	quotas, err := api.engine.broker.quotas(r.Context())
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, http.StatusOK, struct {
		Available        bool              `json:"available"`
		Quotas           []state.QuotaView `json:"quota_groups"`
		PricingSupported bool              `json:"pricing_supported"`
		InputEstimate    string            `json:"input_estimate"`
	}{api.engine.broker.available(), quotas, false, "serialized bytes plus model headroom; not tokenizer-exact"})
}

func (api *controlAPI) calls(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if !state.SystemIDPattern.MatchString(id) {
		api.failure(w, state.ErrSystemNotFound)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	var after int64
	if query.Get("after") != "" {
		after, err = strconv.ParseInt(query.Get("after"), 10, 64)
	}
	if err != nil || after < 0 || len(query) > 1 || (len(query) == 1 && len(query["after"]) != 1) {
		api.failure(w, state.Invalid("after", "invalid model-call cursor"))
		return
	}
	calls, next, err := api.store.ListCalls(r.Context(), localAdministrator, id, after)
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, http.StatusOK, struct {
		Calls []state.ExecutionRecord `json:"calls"`
		Next  int64                   `json:"next,omitempty"`
	}{calls, next})
}

func readCommand(w http.ResponseWriter, r *http.Request, target any) (string, error) {
	key := r.Header.Get("Idempotency-Key")
	if !state.CommandKeyPattern.MatchString(key) {
		return "", state.Invalid("Idempotency-Key", "requires 1-128 letters, digits, dots, colons, underscores, or hyphens")
	}
	if r.URL.RawQuery != "" {
		return "", state.Invalid("query", "command queries are not supported")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", state.Invalid("Content-Type", "must be application/json")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxFrame))
	if err != nil {
		return "", state.Invalid("body", "cannot read JSON body within the 64 KiB limit")
	}
	if body := strings.TrimSpace(string(data)); len(body) == 0 || body[0] != '{' {
		return "", state.Invalid("body", "requires a JSON object")
	}
	if err := protocol.DecodeJSON(data, target); err != nil {
		return "", state.Invalid("body", err.Error())
	}
	return key, nil
}

func (api *controlAPI) create(w http.ResponseWriter, r *http.Request) {
	var command state.CreateSystemCommand
	key, err := readCommand(w, r, &command)
	if err == nil && !state.ConfigName.MatchString(command.Launch) {
		err = state.Invalid("launch", "invalid launch-configuration name")
	}
	if err == nil && command.Goal != nil && (strings.TrimSpace(*command.Goal) == "" || len(*command.Goal) > 32768) {
		err = state.Invalid("goal", "must contain 1-32768 bytes when supplied")
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.CreateSystem(r.Context(), localAdministrator, key, command, api.cfg, api.configID)
	api.systemResponse(w, http.StatusCreated, record, err)
}

func (api *controlAPI) revise(w http.ResponseWriter, r *http.Request) {
	var command state.ReviseSystemCommand
	key, err := readCommand(w, r, &command)
	id := r.PathValue("system_id")
	if err == nil && !state.SystemIDPattern.MatchString(id) {
		err = state.ErrSystemNotFound
	}
	if err == nil && command.ExpectedRevision < 1 {
		err = state.Invalid("expected_revision", "must be positive")
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.ReviseSystem(r.Context(), localAdministrator, key, id, command, api.cfg, api.configID)
	api.systemResponse(w, http.StatusOK, record, err)
}

func (api *controlAPI) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if !state.SystemIDPattern.MatchString(id) {
		api.failure(w, state.ErrSystemNotFound)
		return
	}
	record, err := api.store.GetSystem(r.Context(), localAdministrator, id)
	api.systemResponse(w, http.StatusOK, record, err)
}

func (api *controlAPI) systemResponse(w http.ResponseWriter, status int, record state.SystemRecord, err error) {
	if err == nil {
		record, err = api.cfg.Inspect(record)
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, status, record)
}

func (api *controlAPI) list(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	after := query.Get("after")
	if err != nil || (after != "" && !state.SystemIDPattern.MatchString(after)) || len(query) > 1 ||
		(len(query) == 1 && len(query["after"]) != 1) {
		api.failure(w, state.Invalid("after", "invalid pagination cursor"))
		return
	}
	records, next, err := api.store.ListSystems(r.Context(), localAdministrator, after)
	if err == nil {
		for i := range records {
			records[i], err = api.cfg.Inspect(records[i])
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	api.respond(w, http.StatusOK, struct {
		Systems []state.SystemRecord `json:"systems"`
		Next    string               `json:"next,omitempty"`
	}{records, next})
}
