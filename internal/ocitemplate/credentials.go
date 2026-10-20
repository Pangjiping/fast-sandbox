package ocitemplate

import (
	"fmt"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// BasicCredential keeps OCI metadata and native OverlayBD reads on the same
// authentication contract. Identity tokens alone must not become anonymous reads.
func BasicCredential(username, password, identityToken string) (auth.Credential, error) {
	if username == "" && password == "" && identityToken != "" {
		return auth.EmptyCredential, fmt.Errorf("OCI identitytoken-only credentials are not supported; use anonymous or username/password authentication")
	}
	return auth.Credential{Username: username, Password: password}, nil
}
