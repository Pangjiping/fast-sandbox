package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// shellQuote single-quotes an argument for /bin/sh, escaping embedded
// single quotes.
func shellQuote(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", "'\"'\"'") + "'"
}

// sha256Of returns the hex digest of a byte slice.
func sha256Of(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
