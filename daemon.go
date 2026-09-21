//go:build darwin || linux

package main

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
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	nono "github.com/nolabs-ai/nono-go"
)

const controlTokenEnv = "MICROOPERATOR_CONTROL_TOKEN"

var (
	systemIDPattern   = regexp.MustCompile(`^sys_[a-f0-9]{32}$`)
	commandKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// openOwnedFile refuses symlinks, foreign ownership, and writable-by-others
// files. State files must additionally be private. Checking the opened inode
// avoids trusting a separate stat of a potentially replaced final path.
func openOwnedFile(filename string, flags int, private bool) (*os.File, error) {
	// Nonblocking open lets us reject FIFOs instead of hanging before fstat.
	// It does not change regular-file reads or writes.
	file, err := os.OpenFile(filename, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) ||
		info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.Join(errors.New("file must be regular, owned by this user, and have safe permissions"), file.Close())
	}
	return file, nil
}

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
	lock, err := openOwnedFile(filepath.Join(directory, "daemon.lock"), os.O_CREATE|os.O_RDWR, true)
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
// launch configurations only records administrative metadata: there is no worker,
// provider call, or automatic system creation anywhere in this lifecycle.
func runDaemon(ctx context.Context, configPath string, stdout, stderr io.Writer) (err error) {
	cfg, err := loadConfiguration(configPath, os.LookupEnv)
	if err != nil {
		return err
	}
	token := os.Getenv(controlTokenEnv)
	if len(token) < 32 || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return fmt.Errorf("%s must contain 32-256 printable non-space characters", controlTokenEnv)
	}
	if !supportedSandboxPlatform() || !nono.IsSupported() {
		return errors.New("configured worker profiles require an implemented platform and available nono support; this is not untrusted-code qualification")
	}
	lock, err := lockStateDirectory(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	store, err := openStore(ctx, filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.db.Close()) }()
	configID, err := store.rememberConfiguration(ctx, cfg)
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
	server := &http.Server{
		Handler:           newControlHandler(store, cfg, configID, token, logger),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 8192, ErrorLog: logger,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	if err := writeMessage(stdout, message{Type: "ready", Data: listener.Addr().String()}); err != nil {
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
	store    *stateStore
	cfg      configuration
	configID string
	logger   *log.Logger
}

func newControlHandler(store *stateStore, cfg configuration, configID, token string, logger *log.Logger) http.Handler {
	api := &controlAPI{store, cfg, configID, logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		api.respond(w, http.StatusOK, struct {
			Status           string `json:"status"`
			ExecutionEnabled bool   `json:"execution_enabled"`
		}{"ready", false})
	})
	mux.HandleFunc("GET /v1/launch-configurations", func(w http.ResponseWriter, r *http.Request) {
		type launch struct {
			Name          string       `json:"name"`
			Configuration systemConfig `json:"configuration"`
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		api.failure(w, errSystemNotFound)
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
		// Milestone 1 has a single local administrator, not worker sessions.
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
	var field *fieldError
	status, message := http.StatusInternalServerError, "internal storage error"
	switch {
	case errors.As(err, &field):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, errSystemNotFound):
		status, message = http.StatusNotFound, errSystemNotFound.Error()
	case errors.Is(err, errCommandConflict), errors.Is(err, errRevisionConflict):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusRequestTimeout, "request canceled or deadline exceeded"
	default:
		api.logger.Printf("control operation failed: %v", err)
	}
	api.respond(w, status, map[string]string{"error": message})
}

func readCommand(w http.ResponseWriter, r *http.Request, target any) (string, error) {
	key := r.Header.Get("Idempotency-Key")
	if !commandKeyPattern.MatchString(key) {
		return "", invalid("Idempotency-Key", "requires 1-128 letters, digits, dots, colons, underscores, or hyphens")
	}
	if r.URL.RawQuery != "" {
		return "", invalid("query", "command queries are not supported")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "", invalid("Content-Type", "must be application/json")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFrame))
	if err != nil {
		return "", invalid("body", "cannot read JSON body within the 64 KiB limit")
	}
	if err := decodeJSON(data, target); err != nil {
		return "", invalid("body", err.Error())
	}
	return key, nil
}

func (api *controlAPI) create(w http.ResponseWriter, r *http.Request) {
	var command createSystemCommand
	key, err := readCommand(w, r, &command)
	if err == nil && !configName.MatchString(command.Launch) {
		err = invalid("launch", "invalid launch-configuration name")
	}
	if err == nil && command.Goal != nil && (strings.TrimSpace(*command.Goal) == "" || len(*command.Goal) > 32768) {
		err = invalid("goal", "must contain 1-32768 bytes when supplied")
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.createSystem(r.Context(), localAdministrator, key, command, api.cfg, api.configID)
	api.systemResponse(w, http.StatusCreated, record, err)
}

func (api *controlAPI) revise(w http.ResponseWriter, r *http.Request) {
	var command reviseSystemCommand
	key, err := readCommand(w, r, &command)
	id := r.PathValue("system_id")
	if err == nil && !systemIDPattern.MatchString(id) {
		err = errSystemNotFound
	}
	if err == nil && command.ExpectedRevision < 1 {
		err = invalid("expected_revision", "must be positive")
	}
	if err != nil {
		api.failure(w, err)
		return
	}
	record, err := api.store.reviseSystem(r.Context(), localAdministrator, key, id, command, api.cfg, api.configID)
	api.systemResponse(w, http.StatusOK, record, err)
}

func (api *controlAPI) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("system_id")
	if !systemIDPattern.MatchString(id) {
		api.failure(w, errSystemNotFound)
		return
	}
	record, err := api.store.getSystem(r.Context(), localAdministrator, id)
	api.systemResponse(w, http.StatusOK, record, err)
}

func (api *controlAPI) systemResponse(w http.ResponseWriter, status int, record systemRecord, err error) {
	if err == nil {
		record, err = api.cfg.inspect(record)
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
	if err != nil || (after != "" && !systemIDPattern.MatchString(after)) || len(query) > 1 ||
		(len(query) == 1 && len(query["after"]) != 1) {
		api.failure(w, invalid("after", "invalid pagination cursor"))
		return
	}
	records, next, err := api.store.listSystems(r.Context(), localAdministrator, after)
	if err == nil {
		for i := range records {
			records[i], err = api.cfg.inspect(records[i])
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
		Systems []systemRecord `json:"systems"`
		Next    string         `json:"next,omitempty"`
	}{records, next})
}
