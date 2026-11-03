package database_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CleanSign/cleansign/server/internal/database"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
	"github.com/CleanSign/cleansign/server/internal/testutil/pgtest"
)

// The database clock and the test clock may differ a little, so the tests pass their own "now" to the queue and
// use a point far enough ahead that a freshly queued job (next_run_at = the database's now()) is already runnable.
func later() time.Time { return time.Now().Add(time.Hour) }

type queueEnv struct {
	pool  *pgxpool.Pool
	repo  *database.ContractRepo
	files *database.FileStore
}

func newQueueEnv(t *testing.T) queueEnv {
	t.Helper()
	pool := pgtest.New(t)
	newUser(t, database.NewUserRepo(pool), "u1", "a@b.com")
	return queueEnv{pool: pool, repo: database.NewContractRepo(pool), files: database.NewFileStore(pool)}
}

func (e queueEnv) enqueue(t *testing.T, id string) {
	t.Helper()
	if err := e.repo.Enqueue(ctx, pending(id, "u1"), []byte("pdf-"+id)); err != nil {
		t.Fatalf("Enqueue %s: %v", id, err)
	}
}

type state struct {
	status   string
	attempts int
	leased   bool
}

func (e queueEnv) state(t *testing.T, id string) state {
	t.Helper()
	var s state
	err := e.pool.QueryRow(ctx, `SELECT status::text, attempts, lease_until IS NOT NULL FROM contract WHERE id = $1`, id).
		Scan(&s.status, &s.attempts, &s.leased)
	if err != nil {
		t.Fatalf("read state of %s: %v", id, err)
	}
	return s
}

func (e queueEnv) hasFile(t *testing.T, id string) bool {
	t.Helper()
	_, err := e.files.Get(ctx, id)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("file of %s: %v", id, err)
	}
	return err == nil
}

func TestEnqueueStoresTheFile(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")

	got, err := e.files.Get(ctx, "k1")
	if err != nil || string(got) != "pdf-k1" {
		t.Fatalf("stored file: %q %v", got, err)
	}
	c, err := e.repo.Get(ctx, "u1", "k1")
	if err != nil || c.Status != contract.StatusPending {
		t.Fatalf("contract: %+v %v", c, err)
	}
	if st := e.state(t, "k1"); st.attempts != 0 || st.leased {
		t.Errorf("a new job has no attempts and no lease: %+v", st)
	}

	// a second Enqueue with the same id fails as a whole and leaves the first file alone
	if err := e.repo.Enqueue(ctx, pending("k1", "u1"), []byte("other")); err == nil {
		t.Fatal("a duplicate id must be rejected")
	}
	if got, _ := e.files.Get(ctx, "k1"); string(got) != "pdf-k1" {
		t.Errorf("the first file must be untouched: %q", got)
	}
	if _, err := e.files.Get(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

func TestClaimTakesJobsOldestFirstAndOnce(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	e.enqueue(t, "k2")
	now := later()

	j1, ok, err := e.repo.Claim(ctx, "w1", now, contract.LeaseDuration)
	if err != nil || !ok || j1.ID != "k1" || j1.Attempts != 1 || j1.UserID != "u1" || j1.FileName != "k1.pdf" {
		t.Fatalf("first claim: %+v %v %v", j1, ok, err)
	}
	j2, ok, _ := e.repo.Claim(ctx, "w2", now, contract.LeaseDuration)
	if !ok || j2.ID != "k2" {
		t.Fatalf("second claim must take the other job: %+v %v", j2, ok)
	}
	if _, ok, _ := e.repo.Claim(ctx, "w3", now, contract.LeaseDuration); ok {
		t.Error("nothing is left to claim while both leases are valid")
	}
	if st := e.state(t, "k1"); st.attempts != 1 || !st.leased {
		t.Errorf("claimed job: %+v", st)
	}
}

func TestClaimIgnoresJobsThatWaitedTooLong(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "old")
	if _, err := e.pool.Exec(ctx, `UPDATE contract SET created_at = now() - interval '25 hours' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	e.enqueue(t, "fresh")

	job, ok, err := e.repo.Claim(ctx, "w1", later(), contract.LeaseDuration)
	if err != nil || !ok || job.ID != "fresh" {
		t.Fatalf("only the fresh job is claimed: %+v %v %v", job, ok, err)
	}
	if _, ok, _ := e.repo.Claim(ctx, "w2", later(), contract.LeaseDuration); ok {
		t.Error("the job that waited too long must stay unclaimed until the reaper fails it")
	}
	if res, _ := e.repo.ReapExpired(ctx, time.Now()); res.Failed != 1 {
		t.Errorf("the reaper fails it: %+v", res)
	}
	if st := e.state(t, "old"); st.status != "failed" || st.attempts != 0 {
		t.Errorf("failed without ever running: %+v", st)
	}
}

func TestClaimSkipsLockedRowsUnderConcurrency(t *testing.T) {
	e := newQueueEnv(t)
	const jobs = 6
	for i := 0; i < jobs; i++ {
		e.enqueue(t, "k"+string(rune('0'+i)))
	}
	now := later()

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < jobs+3; w++ { // more workers than jobs
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, ok, err := e.repo.Claim(ctx, "w"+string(rune('a'+w)), now, contract.LeaseDuration)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != jobs {
		t.Errorf("every job must be claimed: %v", claimed)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("job %s was claimed %d times", id, n)
		}
	}
}

func TestClaimIgnoresJobsNotDueYet(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	now := later()
	if _, ok, _ := e.repo.Claim(ctx, "w1", now, contract.LeaseDuration); !ok {
		t.Fatal("claim")
	}
	if ok, err := e.repo.Requeue(ctx, "k1", "w1", now.Add(30*time.Second), false); err != nil || !ok {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	if _, ok, _ := e.repo.Claim(ctx, "w2", now.Add(10*time.Second), contract.LeaseDuration); ok {
		t.Error("a requeued job waits until its next_run_at")
	}
	job, ok, _ := e.repo.Claim(ctx, "w2", now.Add(31*time.Second), contract.LeaseDuration)
	if !ok || job.ID != "k1" || job.Attempts != 2 {
		t.Errorf("after the delay it runs again as attempt 2: %+v %v", job, ok)
	}
}

func TestHeartbeatOnlyForTheOwner(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	now := later()
	e.repo.Claim(ctx, "w1", now, contract.LeaseDuration)

	if ok, err := e.repo.Heartbeat(ctx, "k1", "w1", now.Add(time.Minute)); err != nil || !ok {
		t.Errorf("the owner can extend its lease: %v %v", ok, err)
	}
	if ok, _ := e.repo.Heartbeat(ctx, "k1", "w2", now.Add(time.Minute)); ok {
		t.Error("another worker cannot extend the lease")
	}
	if saved, err := e.repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityLow}, nil); err != nil || !saved {
		t.Fatalf("complete: %v %v", saved, err)
	}
	if ok, _ := e.repo.Heartbeat(ctx, "k1", "w1", now.Add(time.Minute)); ok {
		t.Error("a finished job has no lease to extend")
	}
}

func TestRequeueRefundsTheAttemptOnRequest(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	now := later()
	e.repo.Claim(ctx, "w1", now, contract.LeaseDuration)

	if ok, _ := e.repo.Requeue(ctx, "k1", "w2", now, false); ok {
		t.Error("only the owner can give a job back")
	}
	if ok, err := e.repo.Requeue(ctx, "k1", "w1", now, true); err != nil || !ok {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	if st := e.state(t, "k1"); st.attempts != 0 || st.leased || st.status != "pending" {
		t.Errorf("a refunded requeue is as if it never ran: %+v", st)
	}
	job, ok, _ := e.repo.Claim(ctx, "w2", now.Add(time.Second), contract.LeaseDuration)
	if !ok || job.Attempts != 1 {
		t.Errorf("the next claim is attempt 1 again: %+v %v", job, ok)
	}
}

func TestReapRequeuesAJobWhoseWorkerDisappeared(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	t0 := later()
	e.repo.Claim(ctx, "w1", t0, contract.LeaseDuration)

	res, err := e.repo.ReapExpired(ctx, t0.Add(10*time.Second))
	if err != nil || res.Requeued != 0 || res.Failed != 0 {
		t.Fatalf("a valid lease is left alone: %+v %v", res, err)
	}

	expired := t0.Add(contract.LeaseDuration + time.Second)
	res, err = e.repo.ReapExpired(ctx, expired)
	if err != nil || res.Requeued != 1 || res.Failed != 0 {
		t.Fatalf("an expired lease is requeued: %+v %v", res, err)
	}
	if st := e.state(t, "k1"); st.leased || st.status != "pending" || st.attempts != 1 {
		t.Errorf("requeued job: %+v", st)
	}
	if _, ok, _ := e.repo.Claim(ctx, "w2", expired.Add(10*time.Second), contract.LeaseDuration); ok {
		t.Error("it waits for the retry delay first")
	}
	job, ok, _ := e.repo.Claim(ctx, "w2", expired.Add(contract.RetryDelay(1)+time.Second), contract.LeaseDuration)
	if !ok || job.Attempts != 2 {
		t.Errorf("then another worker takes it as attempt 2: %+v %v", job, ok)
	}
	if !e.hasFile(t, "k1") {
		t.Error("the PDF must stay while the job can still be retried")
	}
}

func TestReapGivesUpWhenAttemptsRunOut(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	now := later()
	for attempt := 1; attempt <= contract.MaxAttempts; attempt++ {
		job, ok, err := e.repo.Claim(ctx, "w", now, contract.LeaseDuration)
		if err != nil || !ok || job.Attempts != attempt {
			t.Fatalf("attempt %d: %+v %v %v", attempt, job, ok, err)
		}
		now = now.Add(contract.LeaseDuration + time.Second) // the worker dies; its lease runs out
		if attempt < contract.MaxAttempts {
			if res, _ := e.repo.ReapExpired(ctx, now); res.Requeued != 1 {
				t.Fatalf("attempt %d should be requeued: %+v", attempt, res)
			}
			now = now.Add(contract.RetryDelay(attempt) + time.Second)
		}
	}
	res, err := e.repo.ReapExpired(ctx, now)
	if err != nil || res.Failed != 1 || res.Requeued != 0 {
		t.Fatalf("the last lost attempt fails the job: %+v %v", res, err)
	}
	c, _ := e.repo.Get(ctx, "u1", "k1")
	if c.Status != contract.StatusFailed || c.ErrorMessage == nil || *c.ErrorMessage != contract.StaleMessage {
		t.Errorf("failed job: %+v", c)
	}
	if e.hasFile(t, "k1") {
		t.Error("the PDF is deleted once the job has failed for good")
	}
}

func TestReapFailsJobsThatWaitedTooLong(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")

	if res, _ := e.repo.ReapExpired(ctx, time.Now().Add(time.Hour)); res.Failed != 0 {
		t.Fatalf("a job that has waited an hour is still fine: %+v", res)
	}
	res, err := e.repo.ReapExpired(ctx, time.Now().Add(contract.QueueExpiry+time.Hour))
	if err != nil || res.Failed != 1 {
		t.Fatalf("expired in the queue: %+v %v", res, err)
	}
	c, _ := e.repo.Get(ctx, "u1", "k1")
	if c.Status != contract.StatusFailed || *c.ErrorMessage != contract.QueueExpiredMessage {
		t.Errorf("expired job: %+v", c)
	}
	if e.hasFile(t, "k1") {
		t.Error("the PDF is deleted with it")
	}
}

func TestReapRemovesStrayFilesOfFinishedContracts(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")
	if _, err := e.pool.Exec(ctx, `UPDATE contract SET status = 'failed' WHERE id = 'k1'`); err != nil { // finished without going through Fail
		t.Fatal(err)
	}
	if _, err := e.repo.ReapExpired(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if e.hasFile(t, "k1") {
		t.Error("no PDF may outlive its analysis")
	}
}

func TestFileIsDeletedWhenTheContractEnds(t *testing.T) {
	e := newQueueEnv(t)
	for _, id := range []string{"done", "failed", "deleted"} {
		e.enqueue(t, id)
	}

	if saved, err := e.repo.Complete(ctx, "done", contract.Result{Grade: contract.SeverityLow}, []contract.Clause{clause("done", 0)}); err != nil || !saved {
		t.Fatalf("complete: %v %v", saved, err)
	}
	if err := e.repo.Fail(ctx, "failed", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repo.Delete(ctx, "u1", "deleted"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"done", "failed", "deleted"} {
		if e.hasFile(t, id) {
			t.Errorf("the PDF of %s must be gone", id)
		}
	}
}

func TestSecondCompletionIsIgnored(t *testing.T) {
	e := newQueueEnv(t)
	e.enqueue(t, "k1")

	if saved, _ := e.repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityLow}, []contract.Clause{clause("k1", 0)}); !saved {
		t.Fatal("the first completion is saved")
	}
	// a slow worker that lost its lease finishes after the job was already completed by another one
	if saved, err := e.repo.Complete(ctx, "k1", contract.Result{Grade: contract.SeverityHigh}, []contract.Clause{clause("k1", 1)}); err != nil || saved {
		t.Errorf("the second completion is discarded: %v %v", saved, err)
	}
	clauses, _ := e.repo.Clauses(ctx, "k1")
	if len(clauses) != 1 {
		t.Errorf("clauses must not be duplicated: %d", len(clauses))
	}
	if c, _ := e.repo.Get(ctx, "u1", "k1"); c.Grade == nil || *c.Grade != contract.SeverityLow {
		t.Errorf("the first result stays: %+v", c.Grade)
	}
	// and a failure report cannot overwrite a completed contract either
	if err := e.repo.Fail(ctx, "k1", "late failure"); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.repo.Get(ctx, "u1", "k1"); c.Status != contract.StatusCompleted {
		t.Errorf("a completed contract stays completed: %v", c.Status)
	}
}
