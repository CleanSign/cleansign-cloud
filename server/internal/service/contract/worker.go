package contract

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
	"github.com/CleanSign/cleansign/server/internal/shared/ids"
)

// Why a running job was cancelled. The worker tells these apart from a real failure.
var (
	errShutdown  = errors.New("worker is shutting down")
	errLeaseLost = errors.New("lease lost")
)

// WorkerConfig configures a Worker. Zero values take the defaults noted below.
type WorkerConfig struct {
	// ID identifies this worker as the owner of the leases it takes. It must be unique per process.
	ID string
	// Concurrency is how many jobs run at once (default 1: analysis is memory hungry).
	Concurrency int
	// PollInterval is how long to wait before looking again when the queue is empty (default 2s).
	PollInterval time.Duration
	// ReapInterval is how often expired leases and stale queue entries are cleaned up (default 30s).
	ReapInterval time.Duration
	// JobTimeout is the time limit of one analysis (default 11 minutes).
	JobTimeout time.Duration
	// ShutdownWait is how long running jobs may keep going after the stop signal before they are handed back (default 30s).
	ShutdownWait time.Duration
	// Lease and HeartbeatInterval default to the domain rules; tests shorten them.
	Lease             time.Duration
	HeartbeatInterval time.Duration
}

func (c WorkerConfig) withDefaults() WorkerConfig {
	if c.ID == "" {
		c.ID = "worker-" + ids.New()
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = 30 * time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 11 * time.Minute
	}
	if c.ShutdownWait <= 0 {
		c.ShutdownWait = 30 * time.Second
	}
	if c.Lease <= 0 {
		c.Lease = contract.LeaseDuration
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = contract.HeartbeatInterval
	}
	return c
}

// Worker takes analysis jobs from the queue and runs them. Any number of workers, in any number of processes, can share one queue.
type Worker struct {
	queue    Queue
	files    FileStore
	repo     Repository
	analyzer Analyzer
	clock    clock.Clock
	log      *slog.Logger
	cfg      WorkerConfig
}

func NewWorker(queue Queue, files FileStore, repo Repository, analyzer Analyzer, clk clock.Clock, log *slog.Logger, cfg WorkerConfig) *Worker {
	return &Worker{queue: queue, files: files, repo: repo, analyzer: analyzer, clock: clk, log: log, cfg: cfg.withDefaults()}
}

// Run works until ctx ends, then stops taking new jobs and lets running ones finish for up to ShutdownWait.
// A job that is still running after that is cancelled and handed back to the queue without using up an attempt.
func (w *Worker) Run(ctx context.Context) {
	// Jobs run on their own context so that the stop signal does not cut them off at once.
	workCtx, cancelWork := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelWork(nil)

	var loops sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		loops.Add(1)
		go func() {
			defer loops.Done()
			w.claimLoop(ctx, workCtx)
		}()
	}
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		w.reapLoop(ctx)
	}()
	w.log.Info("worker started", "id", w.cfg.ID, "concurrency", w.cfg.Concurrency)

	<-ctx.Done()
	w.log.Info("worker stopping: no new jobs; waiting for running ones", "wait", w.cfg.ShutdownWait)
	idle := make(chan struct{})
	go func() {
		loops.Wait()
		close(idle)
	}()
	select {
	case <-idle:
	case <-time.After(w.cfg.ShutdownWait):
		w.log.Warn("shutdown wait is over: handing the running jobs back to the queue")
		cancelWork(errShutdown)
		<-idle
	}
	<-reaped
	w.log.Info("worker stopped", "id", w.cfg.ID)
}

// claimLoop takes one job at a time. stop ends the loop; workCtx is what the jobs themselves run on.
func (w *Worker) claimLoop(stop, workCtx context.Context) {
	for stop.Err() == nil {
		job, ok, err := w.queue.Claim(workCtx, w.cfg.ID, w.clock.Now(), w.cfg.Lease)
		if err != nil {
			w.log.Error("claim a job", "error", err)
		}
		if err != nil || !ok {
			if !sleep(stop, w.cfg.PollInterval) {
				return
			}
			continue
		}
		w.process(workCtx, job)
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	for {
		res, err := w.queue.ReapExpired(ctx, w.clock.Now())
		switch {
		case err != nil && ctx.Err() == nil:
			w.log.Error("reap the queue", "error", err)
		case res.Requeued > 0 || res.Failed > 0:
			w.log.Warn("reaped the queue", "requeued", res.Requeued, "failed", res.Failed)
		}
		if !sleep(ctx, w.cfg.ReapInterval) {
			return
		}
	}
}

// sleep waits for d. It returns false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// process runs one claimed job and records how it ended: completed, failed, or handed back for another attempt.
func (w *Worker) process(workCtx context.Context, job contract.Job) {
	log := w.log.With("contract", job.ID, "attempt", job.Attempts)

	jobCtx, cancel := context.WithCancelCause(workCtx)
	defer cancel(nil)
	stopHeartbeat := w.heartbeat(jobCtx, cancel, job, log)
	defer stopHeartbeat()

	defer func() {
		if r := recover(); r != nil {
			log.Error("analysis panicked", "panic", r)
			w.fail(job.ID, saveFailedMsg, log)
		}
	}()

	pdf, err := w.files.Get(jobCtx, job.ID)
	if errors.Is(err, errs.ErrNotFound) {
		// The PDF is gone (for example a job from before the queue existed). Nothing can be done with it.
		log.Warn("the PDF of a queued job is missing")
		w.fail(job.ID, contract.StaleMessage, log)
		return
	}
	if err != nil {
		log.Error("read the PDF", "error", err)
		w.retryOrFail(job, saveFailedMsg, log)
		return
	}

	timeLimited, cancelLimit := context.WithTimeout(jobCtx, w.cfg.JobTimeout)
	defer cancelLimit()
	analysis, err := w.analyzer.Analyze(timeLimited, job.FileName, pdf, func(p contract.Progress) error {
		return w.repo.UpdateProgress(timeLimited, job.ID, p)
	})
	if err != nil {
		w.analysisFailed(jobCtx, job, err, log)
		return
	}
	w.save(jobCtx, job, analysis, log)
}

// heartbeat keeps the lease alive while the job runs. If the lease is lost, the job is cancelled: someone else owns it now.
func (w *Worker) heartbeat(jobCtx context.Context, cancel context.CancelCauseFunc, job contract.Job, log *slog.Logger) (stop func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(w.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-jobCtx.Done():
				return
			case <-t.C:
				ok, err := w.queue.Heartbeat(jobCtx, job.ID, w.cfg.ID, w.clock.Now().Add(w.cfg.Lease))
				if err != nil {
					// a passing database problem: the lease still has time left, so try again at the next beat
					log.Warn("heartbeat failed", "error", err)
					continue
				}
				if !ok {
					cancel(errLeaseLost)
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// analysisFailed decides what an error from the analyzer means.
func (w *Worker) analysisFailed(jobCtx context.Context, job contract.Job, err error, log *slog.Logger) {
	switch cause := context.Cause(jobCtx); {
	case errors.Is(cause, errLeaseLost):
		log.Warn("lease lost: the job now belongs to someone else (or was deleted); dropping it")
		return
	case errors.Is(cause, errShutdown):
		// We cancelled it because the worker is stopping. That is not the job's fault: do not count the attempt.
		log.Info("handing the job back: the worker is shutting down")
		w.requeue(job, w.clock.Now(), true, log)
		return
	}

	var ae *AnalysisError
	if errors.As(err, &ae) {
		if ae.Retryable {
			log.Warn("analysis failed, may be retried", "reason", ae.Message, "error", ae.Err)
			w.retryOrFail(job, ae.Message, log)
			return
		}
		log.Warn("analysis failed", "reason", ae.Message, "error", ae.Err)
		w.fail(job.ID, ae.Message, log)
		return
	}
	// Not an AnalysisError: the progress update failed (the database is in trouble). Worth another attempt.
	log.Error("analysis stopped by an error", "error", err)
	w.retryOrFail(job, saveFailedMsg, log)
}

func (w *Worker) save(jobCtx context.Context, job contract.Job, analysis contract.Analysis, log *slog.Logger) {
	clauses := contract.ClausesFrom(job.ID, analysis)
	for i := range clauses {
		clauses[i].ID = ids.New()
	}
	// saving must not be cut short by the shutdown signal, or the finished analysis would be thrown away
	ctx, cancel := context.WithTimeout(context.WithoutCancel(jobCtx), 30*time.Second)
	defer cancel()
	saved, err := w.repo.Complete(ctx, job.ID, contract.ResultFrom(analysis), clauses)
	if err != nil {
		log.Error("save the analysis", "error", err)
		w.retryOrFail(job, saveFailedMsg, log)
		return
	}
	if !saved {
		log.Info("the contract was deleted or already finished; result discarded")
	}
}

// retryOrFail gives the job another attempt after a delay, or fails it for good if the attempts are used up.
func (w *Worker) retryOrFail(job contract.Job, message string, log *slog.Logger) {
	if contract.CanRetry(job.Attempts) {
		w.requeue(job, w.clock.Now().Add(contract.RetryDelay(job.Attempts)), false, log)
		return
	}
	log.Warn("giving up: no attempts left", "attempts", job.Attempts)
	w.fail(job.ID, message, log)
}

// requeue and fail use their own context so they still work after the shutdown signal.
func (w *Worker) requeue(job contract.Job, notBefore time.Time, refund bool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	ok, err := w.queue.Requeue(ctx, job.ID, w.cfg.ID, notBefore, refund)
	if err != nil {
		log.Error("hand the job back", "error", err) // the lease will expire and the reaper will requeue it
		return
	}
	if !ok {
		log.Warn("could not hand the job back: it no longer belongs to this worker")
	}
}

func (w *Worker) fail(id, message string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := w.repo.Fail(ctx, id, message); err != nil {
		log.Error("mark the contract failed", "error", err)
	}
}
