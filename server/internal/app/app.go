// Package app wires all layers together (manual DI). It is the only place allowed to know every layer.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/CleanSign/cleansign/server/internal/config"
	"github.com/CleanSign/cleansign/server/internal/database"
	"github.com/CleanSign/cleansign/server/internal/infra/ai"
	"github.com/CleanSign/cleansign/server/internal/infra/mail"
	"github.com/CleanSign/cleansign/server/internal/infra/token"
	"github.com/CleanSign/cleansign/server/internal/server"
	authsvc "github.com/CleanSign/cleansign/server/internal/service/auth"
	contractsvc "github.com/CleanSign/cleansign/server/internal/service/contract"
	usersvc "github.com/CleanSign/cleansign/server/internal/service/user"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/ids"
)

const (
	housekeepingInterval = time.Minute
	httpShutdownGrace    = 10 * time.Second
	// How long a starting process keeps trying to reach the database before it gives up and exits.
	databaseWait         = time.Minute
	analysisShutdownWait = 30 * time.Second
	// The overall limit for an analysis job is the AI server wait time plus slack for saving.
	analysisSaveMargin = time.Minute
)

// Overrides replaces external integrations with fakes in tests. Leave empty to use the real implementations.
type Overrides struct {
	Analyzer contractsvc.Analyzer
	Mailer   authsvc.Mailer
	Clock    clock.Clock
	// TuneWorker changes the worker settings, e.g. to poll faster in tests.
	TuneWorker func(*contractsvc.WorkerConfig)
}

type App struct {
	Echo      *echo.Echo
	Contracts contractsvc.Service
	Auth      authsvc.Service
	// Worker runs the queued analyses. Run starts it inside this process when embedWorker is set.
	Worker *contractsvc.Worker

	pool        *pgxpool.Pool
	log         *slog.Logger
	embedWorker bool
}

// New connects to the DB, applies migrations, and assembles the server.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	pool, err := database.OpenWithRetry(ctx, cfg.DatabaseURL, databaseWait, func(err error) { log.Warn("waiting for the database", "error", err) })
	if err != nil {
		return nil, err
	}
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	log.Info("database ready")
	return Build(pool, cfg, log, Overrides{}), nil
}

// Build assembles the server on top of an already prepared DB pool.
func Build(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger, o Overrides) *App {
	clk := o.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	analyzer := analyzerFor(cfg, o)
	mailer := o.Mailer
	if mailer == nil {
		sender := mail.New(mail.Config{
			Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, User: cfg.SMTP.User, Password: cfg.SMTP.Password, From: cfg.SMTP.From,
		}, log)
		if !sender.Configured() {
			log.Warn("SMTP가 설정되지 않아 인증코드를 메일 대신 로그에 남깁니다 (개발용)")
		}
		mailer = sender
	}

	users := database.NewUserRepo(pool)
	authService := authsvc.NewService(
		database.NewCodeRepo(pool), users, database.NewSessionRepo(pool),
		mailer, token.New(cfg.JWTSecret), clk, cfg.JWTSecret, log,
	)
	userService := usersvc.NewService(users, clk)
	contractRepo := database.NewContractRepo(pool)
	contractService := contractsvc.NewService(contractRepo, contractRepo, clk)
	worker := newWorker(pool, cfg, log, clk, analyzer, o)

	e := server.New(server.Deps{
		Auth: authService, Users: userService, Contracts: contractService, Log: log,
		CookieSecure: cfg.CookieSecure, Health: pool.Ping,
	})
	return &App{Echo: e, Contracts: contractService, Auth: authService, Worker: worker, pool: pool, log: log, embedWorker: cfg.Worker.Embedded}
}

// analyzerFor returns the AI analysis client (a fake in tests).
func analyzerFor(cfg config.Config, o Overrides) contractsvc.Analyzer {
	if o.Analyzer != nil {
		return o.Analyzer
	}
	return ai.New(cfg.AI.URL, cfg.AI.Token, cfg.AI.Timeout)
}

// newWorker builds the analysis worker. The API server runs it inside the process; `server worker` runs it alone.
func newWorker(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger, clk clock.Clock, analyzer contractsvc.Analyzer, o Overrides) *contractsvc.Worker {
	repo := database.NewContractRepo(pool)
	workerCfg := contractsvc.WorkerConfig{
		ID:           newWorkerID(),
		Concurrency:  cfg.Worker.Concurrency,
		JobTimeout:   cfg.AI.Timeout + analysisSaveMargin,
		ShutdownWait: analysisShutdownWait,
	}
	if o.TuneWorker != nil {
		o.TuneWorker(&workerCfg)
	}
	return contractsvc.NewWorker(repo, database.NewFileStore(pool), repo, analyzer, clk, log, workerCfg)
}

// newWorkerID names this process as the owner of the job leases it takes: the host name plus a random suffix.
func newWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return host + "-" + ids.New()[:8]
}

// Run starts the server (and the embedded worker, if enabled). Once ctx ends it stops accepting requests, then lets the
// worker finish or hand back its running analyses before cleaning up.
func (a *App) Run(ctx context.Context, port string) error {
	housekeepingCtx, stopHousekeeping := context.WithCancel(context.Background())
	housekeepingDone := make(chan struct{})
	go func() {
		defer close(housekeepingDone)
		a.housekeeping(housekeepingCtx)
	}()

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	workerDone := make(chan struct{})
	if a.embedWorker {
		go func() {
			defer close(workerDone)
			a.Worker.Run(workerCtx)
		}()
	} else {
		close(workerDone)
		a.log.Info("no embedded worker: analyses are run by separate worker processes")
	}

	serveErr := make(chan error, 1)
	go func() {
		a.log.Info("server listening", "port", port)
		if err := a.Echo.Start(":" + port); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	var runErr error
	select {
	case runErr = <-serveErr:
	case <-ctx.Done():
		a.log.Info("shutting down")
	}

	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer cancelHTTP()
	if err := a.Echo.Shutdown(httpCtx); err != nil {
		a.log.Warn("http shutdown", "error", err)
	}
	stopWorker() // the worker waits for its running analyses on its own (up to analysisShutdownWait)
	<-workerDone

	stopHousekeeping()
	<-housekeepingDone
	a.pool.Close()
	return runErr
}

// housekeeping periodically removes expired auth data. (Lost and stale analyses are cleaned up by the worker's reaper.)
func (a *App) housekeeping(ctx context.Context) {
	run := func() {
		if err := a.Auth.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("purge expired auth data", "error", err)
		}
	}
	run()
	t := time.NewTicker(housekeepingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// PoolForTest returns the pool so integration tests can manipulate state directly.
func (a *App) PoolForTest() *pgxpool.Pool { return a.pool }
