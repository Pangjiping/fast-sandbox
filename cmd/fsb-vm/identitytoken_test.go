package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestIdentityTokenManifestAndOverlayBDHandoff(t *testing.T) {
	setupManifestCache(t)
	global.plainHTTP = true
	raw := cacheTestManifest(t, "token-protected-layer")
	var exchanges atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, r.ParseForm())
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh+a&b=secret" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			exchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-secret","expires_in":60}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-secret" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:team/image:pull"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.Header().Set("Docker-Content-Digest", digest.FromBytes(raw).String())
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	reference := host + "/team/image@" + digest.FromBytes(raw).String()
	cfg, err := parseDockerConfig(fmt.Sprintf(`{"auths":{"%s/team":{"identitytoken":"refresh+a&b=secret"}}}`, host))
	require.NoError(t, err)
	image, err := resolveImage(context.Background(), reference, cfg)
	require.NoError(t, err)
	require.Len(t, image.Lowers, 1)
	require.Positive(t, exchanges.Load())
	handoff, err := overlaybdCredentials(cfg, reference)
	require.NoError(t, err)
	stored, err := marshalCredentials(handoff)
	require.NoError(t, err)
	var decoded dockerConfig
	require.NoError(t, json.Unmarshal(stored, &decoded))
	require.Equal(t, "refresh+a&b=secret", decoded.Auths[host+"/team/image"].IdentityToken)
	require.Empty(t, decoded.Auths[host+"/team/image"].Auth, "refresh tokens must not become Basic credentials")
	path := testManifestCachePath(t, reference, cfg)
	other := dockerConfig{Auths: map[string]authEntry{host + "/team": {IdentityToken: "rotated"}}}
	require.NotEqual(t, path, testManifestCachePath(t, reference, other))
	_, err = resolveImage(context.Background(), reference, other)
	require.Error(t, err, "another token cannot reuse cached manifests")
}
