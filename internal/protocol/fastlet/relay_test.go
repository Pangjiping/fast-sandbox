package fastlet

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateHandlerRelay(t *testing.T) {
	valid := []struct{ method, path string }{
		{"", "/vault/s-a"},
		{"", "/vault/s-a?x=1"},
		{"POST", "/vault/s-a"},
		{"PUT", "/vault/s-a"},
		{"GET", "/vault/s-a"},
		{"DELETE", "/vault/s-a"},
		{"PATCH", "/x/y/z"},
		{"HEAD", "/health"},
		{"OPTIONS", "/"},
	}
	for _, request := range valid {
		require.NoError(t, ValidateHandlerRelay(request.method, request.path), "%s %s", request.method, request.path)
	}

	invalid := []struct{ method, path string }{
		{"TRACE", "/vault/s-a"},
		{"CONNECT", "/vault/s-a"},
		{"get", "/vault/s-a"},
		{"POST", ""},
		{"POST", "vault/s-a"},
		{"POST", "http://127.0.0.1:18080/vault/s-a"},
		{"POST", "/vault/s-a x"},
		{"POST", "/vault/\x7f"},
		{"POST", strings.Repeat("/a", 2048)},
	}
	for _, request := range invalid {
		require.Error(t, ValidateHandlerRelay(request.method, request.path), "%s %q", request.method, request.path)
	}
}
