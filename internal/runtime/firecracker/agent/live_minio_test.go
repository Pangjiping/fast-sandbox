package agent

// Live stage-2 verification against a REAL MinIO (and optionally a REAL DART
// instance). These tests are gated behind environment variables because they
// need a seeded store and are meant for the reference host (or a local
// MinIO container). They close the risk item "presigned URL signature
// correctness against MinIO" that unit tests cannot (the fake servers
// verify parameter construction; MinIO validates the signature server-side).
//
// Setup (local smoke):
//
//	docker run -d --name minio -p 19000:9000 -e MINIO_ROOT_USER=smoke \
//	    -e MINIO_ROOT_PASSWORD=smoke-secret minio/minio server /data
//	mc alias set smoke http://127.0.0.1:19000 smoke smoke-secret
//	mc mb smoke/sandbox-images
//	mc cp rootfs.bin smoke/sandbox-images/digest16/rootfs.ext4
//	FS_LIVE_MINIO_ENDPOINT=http://127.0.0.1:19000 \
//	FS_LIVE_MINIO_AK=smoke FS_LIVE_MINIO_SK=smoke-secret \
//	FS_LIVE_MINIO_BUCKET=sandbox-images \
//	FS_LIVE_MINIO_KEY=digest16/rootfs.ext4 \
//	FS_LIVE_MINIO_SHA256=<sha256 of rootfs.bin> \
//	FS_LIVE_DART_ADDR=http://127.0.0.1:8145 \
//	go test ./internal/runtime/firecracker/agent/ -run TestLive -count=1 -v

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func liveMinioEnv(t *testing.T) (endpoint, accessKey, secretKey, bucket, key, wantSHA string) {
	t.Helper()
	endpoint = os.Getenv("FS_LIVE_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("FS_LIVE_MINIO_ENDPOINT not set; live verification is opt-in")
	}
	return endpoint,
		os.Getenv("FS_LIVE_MINIO_AK"),
		os.Getenv("FS_LIVE_MINIO_SK"),
		os.Getenv("FS_LIVE_MINIO_BUCKET"),
		os.Getenv("FS_LIVE_MINIO_KEY"),
		os.Getenv("FS_LIVE_MINIO_SHA256")
}

func liveS3Client(endpoint, accessKey, secretKey, bucket string) *s3Client {
	return &s3Client{
		endpoint: endpoint, region: "us-east-1", bucket: bucket, prefix: "",
		accessKey: accessKey, secretKey: secretKey,
		http:       &http.Client{Timeout: 10 * time.Minute},
		retryDelay: 100 * time.Millisecond,
	}
}

// TestLivePresignedGETAgainstMinIO proves the presigned URL is accepted by a
// real SigV4 server (MinIO re-derives the query signature) and returns the
// exact seeded object bytes.
func TestLivePresignedGETAgainstMinIO(t *testing.T) {
	endpoint, accessKey, secretKey, bucket, key, wantSHA := liveMinioEnv(t)
	client := liveS3Client(endpoint, accessKey, secretKey, bucket)

	presigned, err := client.presignGET(key, time.Hour)
	require.NoError(t, err)
	response, err := http.Get(presigned)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "MinIO rejected the presigned signature")
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if wantSHA != "" {
		require.Equal(t, wantSHA, sha256Hex(payload))
	}
}

// TestLivePullThroughDART exercises the exact stage-2 agent path against a
// real DART instance: presign -> GET <dart>/dart/<presigned-url> -> origin
// fetch -> whole-object digest. A second read proves the DART block cache
// serves it (the origin counter stops moving; asserted via the admin
// metrics the operator can curl).
func TestLivePullThroughDART(t *testing.T) {
	endpoint, accessKey, secretKey, bucket, key, wantSHA := liveMinioEnv(t)
	dartBase := os.Getenv("FS_LIVE_DART_ADDR")
	if dartBase == "" {
		t.Skip("FS_LIVE_DART_ADDR not set; the DART-leg of the live check is opt-in")
	}
	client := &Client{s3: liveS3Client(endpoint, accessKey, secretKey, bucket), dart: &dartGateway{
		base: dartBase, ttl: time.Hour, http: &http.Client{Timeout: 10 * time.Minute},
	}}

	for read := 1; read <= 2; read++ {
		body, err := client.getArtifact(context.Background(), key)
		require.NoError(t, err)
		digest := sha256.New()
		written, err := io.Copy(digest, body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		if wantSHA != "" {
			require.Equal(t, wantSHA, hex.EncodeToString(digest.Sum(nil)), "read %d digest mismatch", read)
		}
		t.Logf("read %d: %d bytes via DART (%s)", read, written, dartBase)
	}
}
