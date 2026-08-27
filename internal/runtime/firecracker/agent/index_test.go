package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchIndex(t *testing.T) {
	index := imageIndex{
		Image:          testImage,
		ManifestRef:    "s3://" + testBucket + "/" + testPrefix + "/abc123def456/manifest.json",
		ArtifactDigest: strings.Repeat("a", 64),
		UpdatedAt:      "2026-08-27T00:00:00Z",
	}
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	payload, err := json.MarshalIndent(index, "", "  ")
	require.NoError(t, err)
	store.objects[indexKey(testImage)] = payload
	client := testS3Client(t, store)

	got, err := fetchIndex(context.Background(), client, testImage)
	require.NoError(t, err)
	require.Equal(t, index, got)
}

func TestFetchIndexNotFound(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	client := testS3Client(t, store)

	_, err := fetchIndex(context.Background(), client, testImage)
	require.ErrorIs(t, err, ErrObjectNotFound)
}

func TestFetchIndexIncomplete(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	payload, err := json.Marshal(map[string]string{"image": testImage})
	require.NoError(t, err)
	store.objects[indexKey(testImage)] = payload
	client := testS3Client(t, store)

	_, err = fetchIndex(context.Background(), client, testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "incomplete")
}

func TestFetchIndexInvalidJSON(t *testing.T) {
	store := &fakeStore{bucket: testBucket, prefix: testPrefix, objects: map[string][]byte{}}
	store.objects[indexKey(testImage)] = []byte("{not json")
	client := testS3Client(t, store)

	_, err := fetchIndex(context.Background(), client, testImage)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode image index")
}

func TestIndexKeyMatchesCacheKey(t *testing.T) {
	// The index key derivation must match the driver cache key so the
	// addressing chain works without control-plane coordination.
	require.Equal(t, indexKey(testImage), "index/"+imageKey(testImage)+".json")
	require.Len(t, imageKey(testImage), 64)
}

// s3NotFoundServer returns 404 for every path.
func s3NotFoundServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(server.Close)
	return server
}

// clientForEndpoint builds a client without the fake store (used for
// signing/path assertions against a raw server).
func clientForEndpoint(t *testing.T, endpoint, prefix string) *s3Client {
	t.Helper()
	return &s3Client{
		endpoint: endpoint, region: "us-east-1", bucket: testBucket, prefix: prefix,
		accessKey: "test-access-key", secretKey: "test-secret-key",
		http:       &http.Client{},
		retryDelay: time.Millisecond,
	}
}
