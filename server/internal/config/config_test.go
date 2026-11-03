package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{"DATABASE_URL": "postgres://x", "JWT_SECRET": "0123456789abcdef"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "4000" || cfg.AI.URL != "http://localhost:8000" || cfg.AI.Timeout != 600*time.Second || cfg.SMTP.Port != 587 || cfg.CookieSecure {
		t.Errorf("기본값: %+v", cfg)
	}
	if !cfg.Worker.Embedded || cfg.Worker.Concurrency != 1 {
		t.Errorf("worker 기본값은 내장 실행, 동시 1건: %+v", cfg.Worker)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"DATABASE_URL": "postgres://x", "JWT_SECRET": "0123456789abcdef", "PORT": "5000", "COOKIE_SECURE": "TRUE",
		"AI_SERVICE_URL": "http://ai:8000/", "AI_SERVICE_TOKEN": "tok", "AI_SERVICE_TIMEOUT_MS": "1500",
		"SMTP_HOST": "smtp.gmail.com", "SMTP_PORT": "465", "SMTP_USER": "u", "SMTP_PASSWORD": "p", "SMTP_FROM": "f@x.com",
		"WORKER_EMBEDDED": "false", "WORKER_CONCURRENCY": "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "5000" || !cfg.CookieSecure || cfg.AI.URL != "http://ai:8000" || cfg.AI.Token != "tok" ||
		cfg.AI.Timeout != 1500*time.Millisecond || cfg.SMTP.Host != "smtp.gmail.com" || cfg.SMTP.Port != 465 {
		t.Errorf("설정: %+v", cfg)
	}
	if cfg.Worker.Embedded || cfg.Worker.Concurrency != 3 {
		t.Errorf("worker 설정: %+v", cfg.Worker)
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	_, err := load(env(map[string]string{"JWT_SECRET": "short", "SMTP_PORT": "abc", "AI_SERVICE_TIMEOUT_MS": "-1", "WORKER_CONCURRENCY": "0"}))
	if err == nil {
		t.Fatal("오류여야 한다")
	}
	for _, want := range []string{"DATABASE_URL", "JWT_SECRET", "SMTP_PORT", "AI_SERVICE_TIMEOUT_MS", "WORKER_CONCURRENCY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s 문제가 메시지에 있어야 한다: %v", want, err)
		}
	}
}

func TestLoadWorkerDoesNotNeedAPISecrets(t *testing.T) {
	cfg, err := loadFor(env(map[string]string{"DATABASE_URL": "postgres://x"}), false)
	if err != nil {
		t.Fatalf("a worker needs only the database: %v", err)
	}
	if cfg.Worker.HealthPort != "4001" || cfg.Worker.Concurrency != 1 || cfg.AI.URL != "http://localhost:8000" {
		t.Errorf("worker defaults: %+v", cfg)
	}

	cfg, err = loadFor(env(map[string]string{"DATABASE_URL": "postgres://x", "WORKER_HEALTH_PORT": "5001", "WORKER_CONCURRENCY": "2", "AI_SERVICE_URL": "http://ai:8000"}), false)
	if err != nil || cfg.Worker.HealthPort != "5001" || cfg.Worker.Concurrency != 2 || cfg.AI.URL != "http://ai:8000" {
		t.Errorf("worker overrides: %+v %v", cfg, err)
	}

	if _, err := loadFor(env(nil), false); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("the database is still required: %v", err)
	}
	if _, err := load(env(map[string]string{"DATABASE_URL": "postgres://x"})); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Errorf("the API still requires JWT_SECRET: %v", err)
	}
}
