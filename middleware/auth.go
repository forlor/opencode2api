package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"opencode2api/config"
)

func AuthMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// CORS 预检请求（OPTIONS）无需鉴权，直接放行
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			// 如果没有设置 API Key，默认放行
			if len(cfg.Server.APIKeys) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error": "Missing Authorization header"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, `{"error": "Invalid Authorization header format"}`, http.StatusUnauthorized)
				return
			}

			token := strings.TrimSpace(parts[1])
			valid := false
			for _, key := range cfg.Server.APIKeys {
				if subtle.ConstantTimeCompare([]byte(key), []byte(token)) == 1 {
					valid = true
					break
				}
			}

			if !valid {
				http.Error(w, `{"error": "Invalid API Key"}`, http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
