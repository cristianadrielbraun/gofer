package authoperator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const (
	controlCommandPath     = "/v1/auth"
	controlRequestLimit    = 64 << 10
	controlDialTimeout     = 2 * time.Second
	controlCommandDeadline = 2 * time.Minute
)

type controlRequest struct {
	Args []string `json:"args"`
}

type controlResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ControlSocketPath is where a running Gofer listens for auth commands that
// change the database. Its directory is private to the user Gofer runs as,
// which is what keeps other local users out.
func ControlSocketPath(databasePath string) string {
	return filepath.Join(databasePath+".control", "socket")
}

// ListenControl opens the control socket for the database the caller has
// locked. Holding the runtime lock is what makes removing a socket left by a
// Gofer that did not shut down cleanly safe.
func ListenControl(databasePath string) (net.Listener, error) {
	path := ControlSocketPath(databasePath)
	directory := filepath.Dir(path)
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create control directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect control directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("control directory %q is not a directory", directory)
	}
	// Chmod fails unless this user owns the directory, so a directory someone
	// else created is refused rather than trusted.
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, fmt.Errorf("restrict control directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale control socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on control socket: %w", err)
	}
	return listener, nil
}

// ServeControl runs mutating auth commands sent to listener against db, the
// database this server has locked. The returned stop waits for a running
// command to finish and removes the socket.
func ServeControl(listener net.Listener, db *storage.DB) (stop func(context.Context) error) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+controlCommandPath, func(w http.ResponseWriter, r *http.Request) {
		var request controlRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, controlRequestLimit)).Decode(&request); err != nil {
			http.Error(w, "invalid control request", http.StatusBadRequest)
			return
		}
		var stdout, stderr bytes.Buffer
		exitCode := runControlCommand(r.Context(), request.Args, db, &stdout, &stderr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(controlResponse{
			ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String(),
		})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("control socket stopped: %v", err)
		}
	}()
	return server.Shutdown
}

func runControlCommand(ctx context.Context, args []string, db *storage.DB, stdout, stderr io.Writer) int {
	command, help, err := parseCommand(args)
	if help || err != nil || !command.mutating() {
		_, _ = io.WriteString(stderr, "auth: the running Gofer did not recognize this command; it may be an older version\n")
		return 2
	}
	exitCode := runMutation(ctx, command, func(action func(*Service) error) error {
		return action(NewService(db))
	}, stdout, stderr)
	log.Printf("control: auth %s finished with exit code %d", command.name, exitCode)
	return exitCode
}

// runOnServer sends args to a running Gofer. It reports handled=false only
// when no Gofer is listening, so the caller can safely run the command
// itself; once a request is sent it is never retried locally.
func runOnServer(ctx context.Context, socketPath string, args []string, stdout, stderr io.Writer) (handled bool, exitCode int) {
	if _, err := os.Stat(socketPath); err != nil {
		return false, 0
	}
	dialCtx, cancel := context.WithTimeout(ctx, controlDialTimeout)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
	cancel()
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return false, 0 // a socket left behind by a Gofer that is no longer running
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: reach the running Gofer through %s: %v\n", socketPath, err)
		return true, 1
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlCommandDeadline))

	body, err := json.Marshal(controlRequest{Args: args})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: encode command: %v\n", err)
		return true, 1
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gofer"+controlCommandPath, bytes.NewReader(body))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: build command request: %v\n", err)
		return true, 1
	}
	request.Header.Set("Content-Type", "application/json")
	if err := request.Write(conn); err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: send command to the running Gofer: %v\n", err)
		return true, 1
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: read the running Gofer's answer: %v\n", err)
		return true, 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "auth: the running Gofer refused the command (HTTP %d); it may be an older version\n", response.StatusCode)
		return true, 1
	}
	var result controlResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		_, _ = fmt.Fprintf(stderr, "auth: decode the running Gofer's answer: %v\n", err)
		return true, 1
	}
	_, _ = io.WriteString(stdout, result.Stdout)
	_, _ = io.WriteString(stderr, result.Stderr)
	return true, result.ExitCode
}
