package nodecleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Server struct {
	socketPath string
	listener   net.Listener
	httpServer *http.Server
}

func NewServer(socketPath string, cleaner RuntimeProcessCleaner) (*Server, error) {
	if cleaner == nil {
		return nil, errors.New("runtime process cleaner is required")
	}
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return nil, fmt.Errorf("create NodeJanitor control directory: %w", err)
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refuse to replace non-socket NodeJanitor control path %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale NodeJanitor control socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect NodeJanitor control socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on NodeJanitor control socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("protect NodeJanitor control socket: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle(EnsureAbsentPath, ensureAbsentHandler(cleaner))
	return &Server{
		socketPath: socketPath,
		listener:   listener,
		httpServer: &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second},
	}, nil
}

func (s *Server) Serve(ctx context.Context) error {
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownContext)
	}()
	err := s.httpServer.Serve(s.listener)
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func (s *Server) Close() error {
	var result error
	if s.httpServer != nil {
		result = errors.Join(result, s.httpServer.Close())
	}
	if s.socketPath != "" {
		if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func ensureAbsentHandler(cleaner RuntimeProcessCleaner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		defer r.Body.Close()
		var request EnsureAbsentRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if request.Kind == "" || request.SandboxID == "" {
			http.Error(w, "kind and sandboxID are required", http.StatusBadRequest)
			return
		}
		if err := cleaner.EnsureRuntimeProcessesAbsent(r.Context(), request.Kind, request.SandboxID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
