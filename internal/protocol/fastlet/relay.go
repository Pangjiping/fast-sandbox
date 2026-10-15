package fastlet

import (
	"fmt"
	"net/url"
)

// maxHandlerRelayPathBytes bounds the loopback request path of a relayed
// handler invocation.
const maxHandlerRelayPathBytes = 2048

// ValidateHandlerRelay validates the HTTP addressing of a relayed handler
// invocation. It is the shared contract of both relay endpoints: FastPath
// rejects invalid requests before the cross-network hop, and the Fastlet
// re-validates before constructing the loopback request.
func ValidateHandlerRelay(method, path string) error {
	switch method {
	case "", "GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS":
	default:
		return fmt.Errorf("relay method %q is not supported", method)
	}
	if path == "" {
		return fmt.Errorf("relay path is required")
	}
	if len(path) > maxHandlerRelayPathBytes {
		return fmt.Errorf("relay path exceeds %d bytes", maxHandlerRelayPathBytes)
	}
	// The path is spliced into the HTTP request line of the loopback call:
	// only printable ASCII without whitespace can never break the framing or
	// smuggle headers.
	for index := range len(path) {
		if path[index] < 0x21 || path[index] > 0x7e {
			return fmt.Errorf("relay path must contain printable ASCII without whitespace")
		}
	}
	if path[0] != '/' {
		return fmt.Errorf("relay path must start with '/'")
	}
	if _, err := url.ParseRequestURI(path); err != nil {
		return fmt.Errorf("relay path is invalid: %w", err)
	}
	return nil
}
