package agent

// Stage-2 P2P tests: SigV4 query presigning (B) and the DART routing of
// artifact downloads with direct-S3 fallback (C). Metadata (image index,
// manifest) intentionally stays on the direct header-signed path, so its
// 404 semantics (ErrImageNotReady) survive DART's origin-error collapsing.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimecontract "fast-sandbox/internal/runtime/contract"

	"github.com/stretchr/testify/require"
)

// fakeDART stands in for a node-local DART instance: it accepts prefix-mode
// requests (/dart/<full upstream URL>) and proxies them to the origin. The
// gateway can be switched into a 502 "origin error" mode to simulate a
// broken upstream path.
type fakeDART struct {
	mu     sync.Mutex
	server *httptest.Server
	hits   []string
	fail   bool
}

func newFakeDART(t *testing.T) *fakeDART {
	t.Helper()
	gateway := &fakeDART{}
	gateway.server = httptest.NewServer(http.HandlerFunc(gateway.ServeHTTP))
	t.Cleanup(gateway.server.Close)
	return gateway
}

func (d *fakeDART) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uri := r.RequestURI
	d.mu.Lock()
	fail := d.fail
	d.mu.Unlock()
	if fail {
		http.Error(w, "origin error: upstream fetch failed", http.StatusBadGateway)
		return
	}
	if !strings.HasPrefix(uri, "/dart/") {
		http.Error(w, "cannot resolve origin", http.StatusBadRequest)
		return
	}
	upstream := uri[len("/dart/"):]
	if !strings.HasPrefix(upstream, "http://") && !strings.HasPrefix(upstream, "https://") {
		http.Error(w, "upstream URL must include a scheme", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.hits = append(d.hits, upstream)
	d.mu.Unlock()
	response, err := http.Get(upstream)
	if err != nil {
		http.Error(w, "origin error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (d *fakeDART) url() string { return d.server.URL }

// hitCount returns how many prefix-mode requests reached the gateway.
func (d *fakeDART) hitCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.hits)
}

// --- B: presigned URL construction and SigV4 query signature ---------------

func TestPresignGETParametersAndSignature(t *testing.T) {
	var received *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	client := &s3Client{
		endpoint: server.URL, region: "us-east-1", bucket: testBucket, prefix: testPrefix,
		accessKey: "test-access-key", secretKey: "test-secret-key",
		http: &http.Client{Timeout: time.Second},
	}
	presigned, err := client.presignGET("index/abc.json")
	require.NoError(t, err)

	parsed, err := url.Parse(presigned)
	require.NoError(t, err)
	require.Equal(t, "/"+testBucket+"/"+testPrefix+"/index/abc.json", parsed.Path)
	require.NotEmpty(t, parsed.Query().Get("X-Amz-Signature"))

	// The presigned URL works with a plain (unauthenticated) client and
	// carries no Authorization header — the signature is entirely in the
	// query string.
	response, err := http.Get(presigned)
	require.NoError(t, err)
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "ok", string(content))
	require.Empty(t, received.Header.Get("Authorization"), "presigned GET must not carry header signing")

	query := parsed.Query()
	require.Equal(t, "AWS4-HMAC-SHA256", query.Get("X-Amz-Algorithm"))
	require.True(t, strings.HasPrefix(query.Get("X-Amz-Credential"), "test-access-key/"))
	require.Contains(t, query.Get("X-Amz-Credential"), "/us-east-1/s3/aws4_request")
	require.Equal(t, "host", query.Get("X-Amz-SignedHeaders"))
	require.Equal(t, "3600", query.Get("X-Amz-Expires"))
	require.NotEmpty(t, query.Get("X-Amz-Date"))
	require.Len(t, query.Get("X-Amz-Signature"), 64)
}

func TestPresignGETSignatureValidatesServerSide(t *testing.T) {
	// The server re-derives the SigV4 query signature from the received
	// request (the same way MinIO/OSS do) and rejects a mismatch, proving
	// the canonical request is correct, not just well-formed.
	secret := "test-secret-key"
	accessKey := "test-access-key"
	verified := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, verifyPresignedSignature(t, accessKey, secret, "us-east-1", r))
		verified <- true
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	client := &s3Client{
		endpoint: server.URL, region: "us-east-1", bucket: testBucket, prefix: "",
		accessKey: accessKey, secretKey: secret,
		http: &http.Client{Timeout: time.Second},
	}
	presigned, err := client.presignGET("digest16/rootfs.ext4")
	require.NoError(t, err)
	response, err := http.Get(presigned)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.True(t, <-verified)
}

// verifyPresignedSignature recomputes the presigned GET signature from the
// received request and compares it with the X-Amz-Signature parameter.
func verifyPresignedSignature(t *testing.T, accessKey, secretKey, region string, r *http.Request) error {
	t.Helper()
	query := r.URL.Query()
	algorithm := query.Get("X-Amz-Algorithm")
	if algorithm != "AWS4-HMAC-SHA256" {
		t.Fatalf("unexpected algorithm %q", algorithm)
	}
	scope := query.Get("X-Amz-Credential")
	parts := strings.Split(scope, "/")
	if len(parts) != 5 || parts[0] != accessKey || parts[4] != "aws4_request" {
		t.Fatalf("unexpected credential scope %q", scope)
	}
	amzDate := query.Get("X-Amz-Date")
	date := parts[1]
	signedHeaders := query.Get("X-Amz-SignedHeaders")
	if signedHeaders != "host" {
		t.Fatalf("signed headers %q, want host", signedHeaders)
	}
	parameters := [][2]string{
		{"X-Amz-Algorithm", algorithm},
		{"X-Amz-Credential", scope},
		{"X-Amz-Date", amzDate},
		{"X-Amz-Expires", query.Get("X-Amz-Expires")},
		{"X-Amz-SignedHeaders", signedHeaders},
	}
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		r.URL.EscapedPath(),
		canonicalQueryString(parameters),
		"host:" + r.Host + "\n",
		"host",
		unsignedPayload,
	}, "\n")
	scopeValue := date + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scopeValue + "\n" + sha256Hex([]byte(canonicalRequest))
	expected := hmacHex(deriveSigningKey(secretKey, date, region), stringToSign)
	if expected != query.Get("X-Amz-Signature") {
		return &httpError{StatusCode: http.StatusForbidden, Body: "signature mismatch"}
	}
	return nil
}

// --- C: artifact routing through DART with direct-S3 fallback --------------

func TestPullImageArtifactsViaDART(t *testing.T) {
	store, client, _, artifacts := publishFixture(t)
	gateway := newFakeDART(t)
	dartClient := &Client{s3: client, dart: &dartGateway{
		base: gateway.url(), http: &http.Client{Timeout: time.Minute},
	}}
	root := t.TempDir()

	require.NoError(t, dartClient.PullImage(context.Background(), root, testImage))

	dir := imageDir(root, testImage)
	for cacheName, content := range map[string][]byte{
		"rootfs.img":   artifacts["rootfs.ext4"],
		"vmstate.snap": artifacts["vmstate.snap"],
		"memory.snap":  artifacts["memory.snap"],
	} {
		got, err := os.ReadFile(filepath.Join(dir, cacheName))
		require.NoError(t, err)
		require.Equal(t, content, got, cacheName)
	}

	// Every object reached the store exactly once: the index and manifest
	// DIRECT (metadata keeps exact 404 semantics), the three artifacts via
	// the DART proxy forwarding the presigned upstream (a direct fetch would
	// double the artifact keys).
	buildDir := sha256Hex(testManifest(artifacts))[:16]
	require.Equal(t, 1, store.countRequested("index/"+imageKey(testImage)+".json"))
	require.Equal(t, 1, store.countRequested(buildDir+"/manifest.json"))
	require.Equal(t, 1, store.countRequested(buildDir+"/rootfs.ext4"))
	require.Equal(t, 1, store.countRequested(buildDir+"/vmstate.snap"))
	require.Equal(t, 1, store.countRequested(buildDir+"/memory.snap"))

	require.Equal(t, 3, gateway.hitCount(), "three artifacts must go through DART")
	for _, upstream := range gateway.hits {
		parsed, err := url.Parse(upstream)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(upstream, "http://"), upstream)
		query := parsed.Query()
		require.NotEmpty(t, query.Get("X-Amz-Signature"), "DART upstream must be presigned")
		require.Equal(t, "host", query.Get("X-Amz-SignedHeaders"))
		require.NotContains(t, upstream, "index/", "metadata must not go through DART")
		require.NotContains(t, upstream, "manifest.json", "metadata must not go through DART")
	}
}

func TestPullImageDARTUnreachableFallsBackToDirect(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	gateway := newFakeDART(t)
	dartBase := gateway.url()
	gateway.server.Close() // simulate a dead DART process

	dartClient := &Client{s3: client, dart: &dartGateway{
		base: dartBase, http: &http.Client{Timeout: time.Second},
	}}
	require.NoError(t, dartClient.PullImage(context.Background(), t.TempDir(), testImage))

	// Transport failure to DART falls back to the direct path: every object
	// (index + manifest + 3 artifacts) was fetched from the store directly.
	require.Equal(t, 5, len(store.requested()))
	require.Zero(t, gateway.hitCount())
}

func TestPullImageDARTGatewayErrorFallsBackToDirect(t *testing.T) {
	store, client, _, _ := publishFixture(t)
	gateway := newFakeDART(t)
	gateway.mu.Lock()
	gateway.fail = true // DART answers 502 "origin error"
	gateway.mu.Unlock()

	dartClient := &Client{s3: client, dart: &dartGateway{
		base: gateway.url(), http: &http.Client{Timeout: time.Second},
	}}
	require.NoError(t, dartClient.PullImage(context.Background(), t.TempDir(), testImage))

	require.Equal(t, 5, len(store.requested()), "gateway errors must fall back to direct S3")
}

func TestPullImageDARTKeepsImageNotReadySemantics(t *testing.T) {
	// The index lives only in the store, and it is fetched DIRECT even in
	// DART mode: a missing build must keep surfacing ErrImageNotReady
	// (DART would collapse the origin 404 into a 502).
	store, client, _, _ := publishFixture(t)
	store.mu.Lock()
	delete(store.objects, "index/"+imageKey(testImage)+".json")
	store.mu.Unlock()
	gateway := newFakeDART(t)

	dartClient := &Client{s3: client, dart: &dartGateway{
		base: gateway.url(), http: &http.Client{Timeout: time.Second},
	}}
	err := dartClient.PullImage(context.Background(), t.TempDir(), testImage)
	require.ErrorIs(t, err, runtimecontract.ErrImageNotReady)
	require.Zero(t, gateway.hitCount(), "no artifact request may reach DART without an index")
}
