package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CleanSign/cleansign/server/internal/config"
	"github.com/CleanSign/cleansign/server/internal/database"
	contractsvc "github.com/CleanSign/cleansign/server/internal/service/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
)

// WorkerApp is a process that only runs analyses (`server worker`): no API, just a health endpoint for the orchestrator.
type WorkerApp struct {
	Worker *contractsvc.Worker

	pool     *pgxpool.Pool
	log      *slog.Logger
	ownsPool bool
}

// NewWorkerApp connects to the database and assembles a worker process.
// It does not apply migrations: the API server does that, and a worker that starts first just retries until the schema exists.
func NewWorkerApp(ctx context.Context, cfg config.Config, log *slog.Logger) (*WorkerApp, error) {
	pool, err := database.OpenWithRetry(ctx, cfg.DatabaseURL, databaseWait, func(err error) { log.Warn("waiting for the database", "error", err) })
	if err != nil {
		return nil, err
	}
	a := BuildWorker(pool, cfg, log, Overrides{})
	a.ownsPool = true
	return a, nil
}

// BuildWorker assembles a worker process on top of an already prepared DB pool (the caller keeps owning the pool).
func BuildWorker(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger, o Overrides) *WorkerApp {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &WorkerApp{Worker: newWorker(pool, cfg, log, clk, analyzerFor(cfg, o), o), pool: pool, log: log}
}

// Health answers 200 while the database is reachable, 503 otherwise.
func (a *WorkerApp) Health() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := a.pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// Run works until ctx ends: it serves the health endpoint on healthPort and runs the worker, which on the stop signal
// gives its running analyses time to finish or hands them back to the queue before this returns.
func (a *WorkerApp) Run(ctx context.Context, healthPort string) error {
	health := &http.Server{Addr: ":" + healthPort, Handler: a.Health(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		a.log.Info("worker health endpoint listening", "port", healthPort)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		a.Worker.Run(runCtx)
	}()

	var runErr error
	select {
	case runErr = <-serveErr: // the health port could not be opened: do not run half-blind
		stop()
	case <-workerDone:
	}
	<-workerDone

	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer cancel()
	if err := health.Shutdown(shutdownCtx); err != nil {
		a.log.Warn("health endpoint shutdown", "error", err)
	}
	if a.ownsPool {
		a.pool.Close()
	}
	return runErr
}
