package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// TokenPrefix marks a wakil-mcp bearer token so it is recognisable in logs and
// secret scanners.
const TokenPrefix = "wakil_"

// TokenHashPrefix marks a stored token hash.
const TokenHashPrefix = "sha256:"

// tokenBytes is the entropy behind a generated token.
const tokenBytes = 32

// GenerateToken returns a new bearer token and its storage hash. The plaintext
// is returned once, to be shown to the operator, and is never persisted.
func GenerateToken() (token, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating token: %w", err)
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken renders the storage hash for a token. Only this form is written to
// config.json.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return TokenHashPrefix + hex.EncodeToString(sum[:])
}

// VerifyTokenHash reports whether token matches a stored hash, in constant
// time. A malformed or empty hash never matches.
func VerifyTokenHash(token, storedHash string) bool {
	if token == "" || storedHash == "" {
		return false
	}
	computed := HashToken(token)
	if len(computed) != len(storedHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}

// LooksLikeToken reports whether s carries the token prefix. Useful for
// rejecting obviously-wrong credentials early.
func LooksLikeToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix)
}

// MaskToken renders a token for display after creation, keeping only the
// identifying prefix and the last four characters.
func MaskToken(token string) string {
	if len(token) <= len(TokenPrefix)+4 {
		return TokenPrefix + "..."
	}
	return token[:len(TokenPrefix)+4] + "..." + token[len(token)-4:]
}
