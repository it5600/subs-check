package config

import (
	"os"
	"strconv"
)

// Config holds all runtime parameters for substore-ui-go
type Config struct {
	Port         int
	Host         string
	APIKey       string
	CSRFTokenTTL int64 // milliseconds

	SubsCheckHost string
	SubsCheckPort int

	SubStoreHost string
	SubStorePort int
	SubStorePath string

	ConfigPath string
	PublicDir  string

	RateLimitWindow int64 // milliseconds
	RateLimitMax    int

	MaxBodySize   int64
	NodesPageSize int
	NodesMaxPage  int

	CORSAllowedOrigins []string

	SecurityHeaders map[string]string
}

// Load reads configuration from environment variables with sensible defaults
func Load() *Config {
	return &Config{
		Port:         getEnvInt("PORT", 8300),
		Host:         getEnv("HOST", "0.0.0.0"),
		APIKey:       getEnv("API_KEY", "hy584521"),
		CSRFTokenTTL: 60 * 60 * 1000,

		SubsCheckHost: getEnv("SUBS_CHECK_HOST", "127.0.0.1"),
		SubsCheckPort: getEnvInt("SUBS_CHECK_PORT", 8199),

		SubStoreHost: getEnv("SUB_STORE_HOST", "127.0.0.1"),
		SubStorePort: getEnvInt("SUB_STORE_PORT", 8299),
		SubStorePath: getEnv("SUB_STORE_PATH", "/s-7f3K9mQx"),

		ConfigPath: getEnv("SUBS_CONFIG_PATH", "/vol1/1000/Docker/subs-check/config/config.yaml"),
		PublicDir:  getEnv("PUBLIC_DIR", "./public"),

		RateLimitWindow: 60 * 1000,
		RateLimitMax:    getEnvInt("RATE_LIMIT_MAX", 200),

		MaxBodySize:   1024 * 1024,
		NodesPageSize: 100,
		NodesMaxPage:  500,

		CORSAllowedOrigins: []string{
			"https://ui.19840928.xyz",
			"https://ui.19840928.xyz:16666",
		},

		SecurityHeaders: map[string]string{
			"X-Content-Type-Options":    "nosniff",
			"X-Frame-Options":           "DENY",
			"X-XSS-Protection":          "1; mode=block",
			"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
			"Referrer-Policy":           "strict-origin-when-cross-origin",
		},
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
