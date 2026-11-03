package contract

//go:generate go tool mockgen -typed -source=ports.go -destination=ports_mock_test.go -package=contract -self_package=github.com/CleanSign/cleansign/server/internal/service/contract

import (
	"context"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/contract"
)

// Repository is the contract store port. It returns errs.ErrNotFound if the target is missing or not owned by the caller.
type Repository interface {
	Create(ctx context.Context, c contract.Contract) error
	// Get returns the caller's own contract.
	Get(ctx context.Context, userID, id string) (contract.Contract, error)
	Clauses(ctx context.Context, contractID string) ([]contract.Clause, error)
	Page(ctx context.Context, userID string, page, size int) (contract.Page, error)
	// Recent returns only completed and in-progress analyses, newest first (failed excluded).
	Recent(ctx context.Context, userID string, limit int) ([]contract.Summary, error)
	// Delete returns true if it deleted something. Clauses are deleted with it.
	Delete(ctx context.Context, userID, id string) (bool, error)
	UpdateProgress(ctx context.Context, id string, p contract.Progress) error
	// Complete saves the result and clauses in one transaction.
	// If the user deleted the contract during analysis, it saves nothing and returns false.
	Complete(ctx context.Context, id string, r contract.Result, clauses []contract.Clause) (bool, error)
	Fail(ctx context.Context, id, message string) error
}

// Queue is the analysis job queue. The contract table is the queue: see migration 00003.
// A job is claimed with a lease that the owner keeps extending; if the owner dies the lease expires and the job is retried.
type Queue interface {
	// Enqueue saves a new pending contract together with its PDF in one transaction.
	Enqueue(ctx context.Context, c contract.Contract, pdf []byte) error
	// Claim takes the oldest runnable job for worker. ok is false if nothing is runnable.
	Claim(ctx context.Context, worker string, now time.Time, lease time.Duration) (job contract.Job, ok bool, err error)
	// Heartbeat extends the lease. ok is false if worker no longer owns the job.
	Heartbeat(ctx context.Context, id, worker string, until time.Time) (ok bool, err error)
	// Requeue gives a job back to run again at notBefore. refundAttempt takes back the attempt counted at claim time.
	Requeue(ctx context.Context, id, worker string, notBefore time.Time, refundAttempt bool) (ok bool, err error)
	// ReapExpired requeues jobs whose worker disappeared, fails jobs that are out of attempts or waited too long,
	// and removes PDFs of finished contracts.
	ReapExpired(ctx context.Context, now time.Time) (contract.ReapResult, error)
}

// FileStore returns the uploaded PDF of a queued contract (errs.ErrNotFound if it is gone).
// Where the files live is hidden behind this port: today Postgres, possibly an object storage later.
type FileStore interface {
	Get(ctx context.Context, contractID string) ([]byte, error)
}

// AnalysisError carries the reason an analysis failed together with a message to show the user.
// Analyzer returns every failure in this form.
type AnalysisError struct {
	Message string
	Err     error
	// Retryable is true when trying again may succeed: the AI server was unreachable or failed on its side.
	// It is false when the same input would fail again (unreadable PDF, timeout, bad request).
	Retryable bool
}

func (e *AnalysisError) Error() string { return e.Message }
func (e *AnalysisError) Unwrap() error { return e.Err }

// ProgressFunc is called each time the analysis stage changes. Returning an error stops the analysis.
type ProgressFunc func(contract.Progress) error

// Analyzer is the AI analysis server port.
type Analyzer interface {
	Analyze(ctx context.Context, fileName string, pdf []byte, onProgress ProgressFunc) (contract.Analysis, error)
}
