package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CleanSign/cleansign/server/internal/database/sqlc"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
)

// reapBatch is how many expired leases one reaper pass requeues. The next pass takes the rest.
const reapBatch = 100

// Enqueue stores a new pending contract together with its PDF in one transaction, so a queued job always has its file.
func (r *ContractRepo) Enqueue(ctx context.Context, c contract.Contract, pdf []byte) error {
	progress, err := marshalProgress(c.Progress)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is ignored

	q := r.q.WithTx(tx)
	if err := q.CreateContract(ctx, sqlc.CreateContractParams{
		ID: c.ID, UserID: c.UserID, FileName: c.FileName, MimeType: c.MimeType, Progress: progress,
	}); err != nil {
		return err
	}
	if err := q.InsertContractFile(ctx, sqlc.InsertContractFileParams{ContractID: c.ID, Data: pdf}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Claim takes the oldest runnable job for worker and gives it a lease of the given length. ok is false if nothing is runnable.
// A job that has waited longer than contract.QueueExpiry is not runnable any more; ReapExpired fails it.
func (r *ContractRepo) Claim(ctx context.Context, worker string, now time.Time, lease time.Duration) (job contract.Job, ok bool, err error) {
	until := now.Add(lease)
	row, err := r.q.ClaimContract(ctx, sqlc.ClaimContractParams{
		Now: now, LeaseUntil: &until, Worker: &worker, MaxAttempts: contract.MaxAttempts,
		NotBefore: now.Add(-contract.QueueExpiry),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.Job{}, false, nil
	}
	if err != nil {
		return contract.Job{}, false, err
	}
	return contract.Job{ID: row.ID, UserID: row.UserID, FileName: row.FileName, Attempts: int(row.Attempts)}, true, nil
}

// Heartbeat extends the lease. ok is false if worker no longer owns the job.
func (r *ContractRepo) Heartbeat(ctx context.Context, id, worker string, until time.Time) (bool, error) {
	n, err := r.q.HeartbeatContract(ctx, sqlc.HeartbeatContractParams{ID: id, Worker: &worker, LeaseUntil: &until})
	return n > 0, err
}

// Requeue gives a job back so it runs again at notBefore. refundAttempt takes back the attempt that was counted when it was
// claimed (used when the job was interrupted by a shutdown rather than failing). ok is false if worker no longer owns the job.
func (r *ContractRepo) Requeue(ctx context.Context, id, worker string, notBefore time.Time, refundAttempt bool) (bool, error) {
	progress, err := marshalProgress(initialProgress())
	if err != nil {
		return false, err
	}
	var refund int32
	if refundAttempt {
		refund = 1
	}
	n, err := r.q.RequeueContract(ctx, sqlc.RequeueContractParams{
		ID: id, Worker: &worker, NextRunAt: notBefore, Refund: refund, Progress: progress,
	})
	return n > 0, err
}

// ReapExpired cleans up the queue: jobs whose worker disappeared are requeued (or failed if their attempts are used up),
// contracts that waited too long are failed, and no PDF is left behind for a finished contract.
func (r *ContractRepo) ReapExpired(ctx context.Context, now time.Time) (contract.ReapResult, error) {
	var res contract.ReapResult
	lost := contract.StaleMessage
	maxAttempts := int32(contract.MaxAttempts)

	n, err := r.q.FailExhaustedLeases(ctx, sqlc.FailExhaustedLeasesParams{Now: &now, MaxAttempts: maxAttempts, Message: &lost})
	if err != nil {
		return res, err
	}
	res.Failed += int(n)

	expired, err := r.q.ListExpiredLeases(ctx, sqlc.ListExpiredLeasesParams{Now: &now, MaxAttempts: maxAttempts, Batch: reapBatch})
	if err != nil {
		return res, err
	}
	progress, err := marshalProgress(initialProgress())
	if err != nil {
		return res, err
	}
	for _, e := range expired {
		n, err := r.q.RequeueExpiredLease(ctx, sqlc.RequeueExpiredLeaseParams{
			ID: e.ID, Now: &now, NextRunAt: now.Add(contract.RetryDelay(int(e.Attempts))), Progress: progress,
		})
		if err != nil {
			return res, err
		}
		res.Requeued += int(n)
	}

	waited := contract.QueueExpiredMessage
	n, err = r.q.FailExpiredQueued(ctx, sqlc.FailExpiredQueuedParams{CreatedBefore: now.Add(-contract.QueueExpiry), Message: &waited})
	if err != nil {
		return res, err
	}
	res.Failed += int(n)

	if _, err := r.q.DeleteFinishedContractFiles(ctx); err != nil {
		return res, err
	}
	return res, nil
}

func initialProgress() *contract.Progress {
	p := contract.InitialProgress()
	return &p
}

// FileStore keeps the uploaded PDFs in Postgres (table contract_file). It is the only place that knows where files live:
// an object storage implementation can replace it later without touching the worker.
type FileStore struct {
	q *sqlc.Queries
}

func NewFileStore(pool *pgxpool.Pool) *FileStore { return &FileStore{q: queries(pool)} }

// Get returns the PDF of a contract, or errs.ErrNotFound if it is gone (deleted, expired, or the analysis already finished).
func (s *FileStore) Get(ctx context.Context, contractID string) ([]byte, error) {
	data, err := s.q.GetContractFile(ctx, contractID)
	if err != nil {
		return nil, notFound(err)
	}
	return data, nil
}
