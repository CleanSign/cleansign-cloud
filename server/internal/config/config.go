// Package config reads server settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

type AI struct {
	URL     string
	Token   string
	Timeout time.Duration
}

// Worker configures the analysis worker.
type Worker struct {
	// Embedded runs a worker inside the API server process. Turn it off when workers run as their own processes.
	Embedded bool
	// Concurrency is how many analyses one worker runs at once.
	Concurrency int
	// HealthPort is where a separate worker process (`server worker`) answers /healthz.
	HealthPort string
}

type Config struct {
	Port         string
	DatabaseURL  string
	JWTSecret    string
	CookieSecure bool
	SMTP         SMTP
	AI           AI
	Worker       Worker
}

const minSecretLen = 16

// Load reads the settings of the API server and reports what is wrong if a required value is missing.
func Load() (Config, error) { return load(os.Getenv) }

// LoadWorker reads the settings of a separate worker process. It needs the database and the AI server,
// but not the secrets that only the API uses (JWT, SMTP).
func LoadWorker() (Config, error) { return loadFor(os.Getenv, false) }

func load(get func(string) string) (Config, error) { return loadFor(get, true) }

func loadFor(get func(string) string, api bool) (Config, error) {
	var problems []string
	str := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}
	integer := func(key string, def int) int {
		raw := strings.TrimSpace(get(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			problems = append(problems, fmt.Sprintf("%s는 양의 정수여야 해요 (현재 %q)", key, raw))
			return def
		}
		return n
	}

	cfg := Config{
		Port:         str("PORT", "4000"),
		DatabaseURL:  str("DATABASE_URL", ""),
		JWTSecret:    str("JWT_SECRET", ""),
		CookieSecure: strings.EqualFold(str("COOKIE_SECURE", "false"), "true"),
		SMTP: SMTP{
			Host:     str("SMTP_HOST", ""),
			Port:     integer("SMTP_PORT", 587),
			User:     str("SMTP_USER", ""),
			Password: str("SMTP_PASSWORD", ""),
			From:     str("SMTP_FROM", ""),
		},
		AI: AI{
			URL:     strings.TrimRight(str("AI_SERVICE_URL", "http://localhost:8000"), "/"),
			Token:   str("AI_SERVICE_TOKEN", ""),
			Timeout: time.Duration(integer("AI_SERVICE_TIMEOUT_MS", 600_000)) * time.Millisecond,
		},
		Worker: Worker{
			Embedded:    !strings.EqualFold(str("WORKER_EMBEDDED", "true"), "false"),
			Concurrency: integer("WORKER_CONCURRENCY", 1),
			HealthPort:  str("WORKER_HEALTH_PORT", "4001"),
		},
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL이 필요해요")
	}
	if api && len(cfg.JWTSecret) < minSecretLen {
		problems = append(problems, fmt.Sprintf("JWT_SECRET이 필요해요 (%d자 이상)", minSecretLen))
	}
	if len(problems) > 0 {
		return Config{}, errors.New("설정 오류:\n  - " + strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}
