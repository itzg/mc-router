package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

func apiKeyMiddleware(next http.Handler, expectedKey string) http.Handler {
	// An empty key disables authentication.
	if expectedKey == "" {
		return next
	}

	expectedHash := sha256.Sum256([]byte(expectedKey))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providedKey := r.Header.Get("X-API-Key")
		providedHash := sha256.Sum256([]byte(providedKey))

		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}