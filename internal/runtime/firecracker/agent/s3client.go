package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fast-sandbox/internal/registryconfig"
)

// ErrObjectNotFound reports that the requested object does not exist in the
// store (S3 404 / NoSuchKey).
var ErrObjectNotFound = errors.New("object not found")

// httpError carries an unexpected S3 response status with a bounded body.
type httpError struct {
	StatusCode int
	Body       string
}

func (e *httpError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("S3 GET failed with status %d: %s", e.StatusCode, body)
}

const (
	// defaultS3Region is the SigV4 signing region when none is configured.
	// S3-compatible stores with endpoint overrides accept it regardless of
	// their real region.
	defaultS3Region = "us-east-1"
	// defaultS3RequestTimeout bounds a single GET. Rootfs images are
	// multi-GiB, so the default is generous.
	defaultS3RequestTimeout = 5 * time.Minute
	// s3RetryAttempts is how many times a transient GET failure is retried
	// with exponential backoff (2s, 4s, 8s).
	s3RetryAttempts = 3
)

// s3Client implements path-style, SigV4-signed GETs against an
// S3-compatible store (AWS S3, Aliyun OSS, MinIO) with a read-only access
// key pair. Only the GET verb needed by the pull layer is implemented;
// range GETs arrive with the overlaybd stage.
type s3Client struct {
	endpoint  string // scheme://host[:port], e.g. https://oss-cn-hangzhou.aliyuncs.com
	region    string
	bucket    string
	prefix    string // store key prefix under the bucket, "" when the root is the bucket
	accessKey string
	secretKey string
	http      *http.Client
	// retryDelay is the base of the exponential backoff for transient
	// failures; it is a field (not a constant) so tests can shorten it.
	retryDelay time.Duration
}

// newS3Client parses the store root (s3://bucket/prefix) and the read-only
// credential into a GET client. The credential Host names the store endpoint
// (matching key); the optional credential Endpoint overrides the connection
// address with a scheme/port (e.g. a local MinIO), falling back to Host.
func newS3Client(storeRoot string, credential registryconfig.Credential, httpClient *http.Client, region, endpointOverride string) (*s3Client, error) {
	bucket, prefix, err := parseStoreRoot(storeRoot)
	if err != nil {
		return nil, err
	}
	endpoint := endpointOverride
	if endpoint == "" {
		endpoint = credential.Endpoint
	}
	if endpoint == "" {
		endpoint = credential.Host
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("S3 endpoint is required (credential host or WithEndpoint)")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid S3 endpoint %q: %w", endpoint, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q: missing host", endpoint)
	}
	if region == "" {
		region = defaultS3Region
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultS3RequestTimeout}
	}
	return &s3Client{
		endpoint: endpoint, region: region, bucket: bucket, prefix: strings.Trim(prefix, "/"),
		accessKey: credential.Username, secretKey: credential.Password,
		http: httpClient, retryDelay: 2 * time.Second,
	}, nil
}

// parseStoreRoot splits "s3://bucket/prefix" into its bucket and key prefix.
func parseStoreRoot(storeRoot string) (bucket, prefix string, err error) {
	parsed, err := url.Parse(storeRoot)
	if err != nil {
		return "", "", fmt.Errorf("invalid store root %q: %w", storeRoot, err)
	}
	if parsed.Scheme != "s3" {
		return "", "", fmt.Errorf("invalid store root %q: scheme must be s3://", storeRoot)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("invalid store root %q: bucket is required", storeRoot)
	}
	return parsed.Host, strings.Trim(parsed.Path, "/"), nil
}

// resolveRef converts a content-addressed object reference
// (s3://bucket/prefix/...) from an index document into a store-relative key,
// rejecting references outside the configured store root.
func (c *s3Client) resolveRef(ref string) (string, error) {
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || parsed.Path == "" {
		return "", fmt.Errorf("invalid object reference %q", ref)
	}
	if parsed.Host != c.bucket {
		return "", fmt.Errorf("object reference %q points at bucket %q, expected %q", ref, parsed.Host, c.bucket)
	}
	key := strings.Trim(parsed.Path, "/")
	if c.prefix != "" {
		if key == c.prefix {
			return "", fmt.Errorf("object reference %q has no object key", ref)
		}
		if !strings.HasPrefix(key, c.prefix+"/") {
			return "", fmt.Errorf("object reference %q points outside store prefix %q", ref, c.prefix)
		}
		key = strings.TrimPrefix(key, c.prefix+"/")
	}
	if key == "" {
		return "", fmt.Errorf("invalid object reference %q", ref)
	}
	return key, nil
}

// get streams the object at a store-relative key. The caller owns the
// returned body and must close it. Transport errors and 5xx responses are
// retried with exponential backoff; 4xx responses (including 404) are not.
func (c *s3Client) get(ctx context.Context, storeKey string) (io.ReadCloser, error) {
	key := storeKey
	if c.prefix != "" {
		key = c.prefix + "/" + storeKey
	}
	urlString, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	backoff := c.retryDelay
	var lastErr error
	for attempt := 0; attempt <= s3RetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
		}
		body, err := c.getOnce(ctx, urlString, key)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, ErrObjectNotFound) {
			return nil, err
		}
		var status *httpError
		if errors.As(err, &status) && status.StatusCode < http.StatusInternalServerError {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("GET %s: %w", key, lastErr)
}

// getOnce performs a single signed GET and returns the response body on 200.
func (c *s3Client) getOnce(ctx context.Context, urlString, key string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, urlString, nil)
	if err != nil {
		return nil, err
	}
	c.sign(request)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, &httpError{StatusCode: response.StatusCode, Body: string(body)}
	}
	return response.Body, nil
}

// objectURL returns the path-style URL of a store object.
func (c *s3Client) objectURL(key string) (string, error) {
	base, err := url.Parse(c.endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid store endpoint %q: %w", c.endpoint, err)
	}
	base.Path = "/" + c.bucket + "/" + key
	return base.String(), nil
}

// emptyPayloadHash is the SHA-256 of an empty payload, the fixed
// X-Amz-Content-Sha256 of a bodyless GET.
var emptyPayloadHash = sha256Hex(nil)

// unsignedPayload is the presigned-URL payload hash. A presigned GET cannot
// promise the caller will set a content hash header, so the signature covers
// the constant "UNSIGNED-PAYLOAD" instead (S3/MinIO convention for query
// signing; the request still carries no body).
const unsignedPayload = "UNSIGNED-PAYLOAD"

// presignGET returns a SigV4 query-signed GET URL for a store object, valid
// for ttl. The signature is derived from the same canonical request as the
// header signing in sign (identical path, host, region scope and signing
// key); only the carrier differs — the X-Amz-* parameters travel in the
// query string and the only signed header is host. Presigned URLs are handed
// to DART so the P2P layer can fetch from the origin without ever seeing the
// access key pair.
func (c *s3Client) presignGET(storeKey string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	key := storeKey
	if c.prefix != "" {
		key = c.prefix + "/" + storeKey
	}
	urlString, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(urlString)
	if err != nil {
		return "", fmt.Errorf("build object URL: %w", err)
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + c.region + "/s3/aws4_request"

	// The five X-Amz-* parameters sort alphabetically in this order.
	parameters := [][2]string{
		{"X-Amz-Algorithm", "AWS4-HMAC-SHA256"},
		{"X-Amz-Credential", c.accessKey + "/" + scope},
		{"X-Amz-Date", amzDate},
		{"X-Amz-Expires", strconv.FormatInt(int64(ttl/time.Second), 10)},
		{"X-Amz-SignedHeaders", "host"},
	}
	canonicalQuery := canonicalQueryString(parameters)
	canonicalHeaders := "host:" + target.Host + "\n"
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		target.EscapedPath(),
		canonicalQuery,
		canonicalHeaders,
		"host",
		unsignedPayload,
	}, "\n")
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))
	signingKey := deriveSigningKey(c.secretKey, date, c.region)
	signature := hmacHex(signingKey, stringToSign)
	target.RawQuery = canonicalQuery + "&X-Amz-Signature=" + signature
	return target.String(), nil
}

// canonicalQueryString renders an already-sorted parameter list as the SigV4
// canonical query string: every name and value is RFC 3986 encoded (spaces as
// %20, never '+') and pairs join with '&'.
func canonicalQueryString(parameters [][2]string) string {
	parts := make([]string, 0, len(parameters))
	for _, pair := range parameters {
		parts = append(parts, awsQueryEscape(pair[0])+"="+awsQueryEscape(pair[1]))
	}
	return strings.Join(parts, "&")
}

// awsQueryEscape encodes a SigV4 query component. url.QueryEscape uses '+' for
// spaces, which SigV4 canonicalization forbids; the '+'-to-%20 replacement
// yields the RFC 3986 form both sides of the signature agree on.
func awsQueryEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// sign applies AWS Signature Version 4 to the request (service s3,
// path-style). The payload hash is the empty-string digest: GETs carry no
// body.
func (c *s3Client) sign(request *http.Request) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")

	request.Header.Set("x-amz-date", amzDate)
	request.Header.Set("x-amz-content-sha256", emptyPayloadHash)

	canonicalURI := request.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	host := request.URL.Host
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + emptyPayloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalURI,
		request.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		emptyPayloadHash,
	}, "\n")
	scope := date + "/" + c.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	signingKey := deriveSigningKey(c.secretKey, date, c.region)
	signature := hmacHex(signingKey, stringToSign)
	request.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+c.accessKey+"/"+scope+
			", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// deriveSigningKey builds the SigV4 per-request signing key.
func deriveSigningKey(secretKey, date, region string) []byte {
	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	return hmacSHA256(key, "aws4_request")
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func hmacHex(key []byte, value string) string {
	return hex.EncodeToString(hmacSHA256(key, value))
}

// sha256Hex returns the lowercase hex SHA-256 of a payload.
func sha256Hex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
