package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

// serveOCILayout serves staged blobs on loopback, so fsb-vm and the convertor
// reuse their registry interface without pulling the source a second time.
func serveOCILayout(workdir string) (string, func(), error) {
	dir := filepath.Join(workdir, "oci-layout")
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		return "", nil, err
	}
	var index oci.Index
	if err = json.Unmarshal(raw, &index); err != nil {
		return "", nil, err
	}
	if len(index.Manifests) != 1 {
		return "", nil, fmt.Errorf("staged OCI layout must contain exactly one image")
	}
	image := index.Manifests[0]
	if err = image.Digest.Validate(); err != nil {
		return "", nil, err
	}
	if image.Digest.Algorithm() != digest.SHA256 {
		return "", nil, fmt.Errorf("staged OCI image requires SHA256")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var reference string
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/source/manifests/"):
			reference = strings.TrimPrefix(r.URL.Path, "/v2/source/manifests/")
			if reference != image.Digest.String() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", image.MediaType)
		case strings.HasPrefix(r.URL.Path, "/v2/source/blobs/"):
			reference = strings.TrimPrefix(r.URL.Path, "/v2/source/blobs/")
			w.Header().Set("Content-Type", "application/octet-stream")
		default:
			http.NotFound(w, r)
			return
		}
		sum := digest.Digest(reference)
		if sum.Validate() != nil || sum.Algorithm() != digest.SHA256 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", reference)
		http.ServeFile(w, r, filepath.Join(dir, "blobs", "sha256", sum.Encoded()))
	})}
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String() + "/source@" + image.Digest.String(), func() { _ = server.Close() }, nil
}
