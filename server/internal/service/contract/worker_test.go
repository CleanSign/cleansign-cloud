package contract

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
)

// --- fakes ---

type queuedJob struct {
	job     contract.Job
	nextRun time.Time
	leased  bool
}

type requeueCall struct {
	id        string
	notBefore time.Time
	refund    bool
}

// fakeQueue is an in-memory Queue: just enough of the real semantics (lease, owner, next run) for the worker's decisions.
type enqueuedContract struct {
	contract contract.Contract
	pdf      []byte
}

type fakeQueue struct {
	mu          sync.Mutex
	jobs        []*queuedJob
	heartbeats  int
	heartbeatOK bool
	requeues    []requeueCall
	reaps       int
	owner       string

	repo       *fakeRepo // if set, Enqueue also creates the contract there, like the real one
	enqueueErr error
	enqueueLog []enqueuedContract
}

func newFakeQueue(jobs ...contract.Job) *fakeQueue {
	q := &fakeQueue{heartbeatOK: true}
	for _, j := range jobs {
		q.jobs = append(q.jobs, &queuedJob{job: j})
	}
	return q
}

func (q *fakeQueue) Enqueue(ctx context.Context, c contract.Contract, pdf []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	q.enqueueLog = append(q.enqueueLog, enqueuedContract{c, pdf})
	if q.repo != nil {
		return q.repo.Create(ctx, c)
	}
	return nil
}

func (q *fakeQueue) enqueued() []enqueuedContract {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]enqueuedContract(nil), q.enqueueLog...)
}

func (q *fakeQueue) Claim(_ context.Context, worker string, now time.Time, _ time.Duration) (contract.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if !j.leased && !j.nextRun.After(now) {
			j.leased = true
			j.job.Attempts++
			q.owner = worker
			return j.job, true, nil
		}
	}
	return contract.Job{}, false, nil
}

func (q *fakeQueue) Heartbeat(context.Context, string, string, time.Time) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeats++
	return q.heartbeatOK, nil
}

func (q *fakeQueue) Requeue(_ context.Context, id, _ string, notBefore time.Time, refund bool) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requeues = append(q.requeues, requeueCall{id, notBefore, refund})
	for _, j := range q.jobs {
		if j.job.ID == id {
			j.leased, j.nextRun = false, notBefore
			if refund {
				j.job.Attempts--
			}
		}
	}
	return true, nil
}

func (q *fakeQueue) ReapExpired(context.Context, time.Time) (contract.ReapResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reaps++
	return contract.ReapResult{}, nil
}

func (q *fakeQueue) setHeartbeatOK(ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeatOK = ok
}

func (q *fakeQueue) counts() (heartbeats, reaps int, requeues []requeueCall) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.heartbeats, q.reaps, append([]requeueCall(nil), q.requeues...)
}

type fakeFiles struct{ missing map[string]bool }

func (f fakeFiles) Get(_ context.Context, id string) ([]byte, error) {
	if f.missing[id] {
		return nil, errs.ErrNotFound
	}
	return []byte("%PDF-" + id), nil
}

type analyzerFunc func(ctx context.Context, fileName string, pdf []byte, onProgress ProgressFunc) (contract.Analysis, error)

func (f analyzerFunc) Analyze(ctx context.Context, fileName string, pdf []byte, p ProgressFunc) (contract.Analysis, error) {
	return f(ctx, fileName, pdf, p)
}

func okAnalyzer(_ context.Context, _ string, _ []byte, on ProgressFunc) (contract.Analysis, error) {
	if err := on(contract.Progress{Stage: "classifying", Done: 1, Total: 2}); err != nil {
		return contract.Analysis{}, err
	}
	return sampleResult(), nil
}

// --- harness ---

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// settableClock is a clock the test moves forward by hand (safe to read from the worker's goroutines).
type settableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *settableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *settableClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type harness struct {
	t     *testing.T
	repo  *fakeRepo
	queue *fakeQueue
	clock *settableClock
	w     *Worker
}

func newHarness(t *testing.T, a Analyzer, files fakeFiles, cfg WorkerConfig, jobs ...contract.Job) *harness {
	t.Helper()
	repo := newFakeRepo()
	for _, j := range jobs {
		repo.Create(context.Background(), contract.Contract{ID: j.ID, UserID: j.UserID, FileName: j.FileName, Status: contract.StatusPending})
	}
	q := newFakeQueue(jobs...)
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Millisecond
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = time.Hour
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Hour // off unless a test asks for it
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clk := &settableClock{t: testNow}
	w := NewWorker(q, files, repo, a, clk, log, cfg)
	return &harness{t: t, repo: repo, queue: q, clock: clk, w: w}
}

// start runs the worker; stop ends it and waits (at most 5s) for Run to return.
func (h *harness) start() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				h.t.Error("Run did not return")
			}
		})
	}
	h.t.Cleanup(stop)
	return stop
}

func (h *harness) status(id string) contract.Status {
	c, _ := h.repo.status(id)
	return c.Status
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func job(id string) contract.Job { return contract.Job{ID: id, UserID: "u1", FileName: id + ".pdf"} }

// --- tests ---

func TestWorkerCompletesAJob(t *testing.T) {
	h := newHarness(t, analyzerFunc(okAnalyzer), fakeFiles{}, WorkerConfig{}, job("k1"))
	h.start()

	eventually(t, "contract completed", func() bool { return h.status("k1") == contract.StatusCompleted })
	if cl, _ := h.repo.Clauses(context.Background(), "k1"); len(cl) != 2 || cl[0].ID == "" || cl[0].ID == cl[1].ID {
		t.Errorf("clauses are saved with their own ids: %+v", cl)
	}
	if _, _, requeues := h.queue.counts(); len(requeues) != 0 {
		t.Errorf("a finished job is not requeued: %+v", requeues)
	}
}

func TestWorkerFailsAJobThatCannotSucceed(t *testing.T) {
	a := analyzerFunc(func(context.Context, string, []byte, ProgressFunc) (contract.Analysis, error) {
		return contract.Analysis{}, &AnalysisError{Message: "스캔본이에요", Retryable: false}
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{}, job("k1"))
	h.start()

	eventually(t, "contract failed", func() bool { return h.status("k1") == contract.StatusFailed })
	c, _ := h.repo.status("k1")
	if c.ErrorMessage == nil || *c.ErrorMessage != "스캔본이에요" {
		t.Errorf("the user sees the reason: %v", c.ErrorMessage)
	}
	if _, _, requeues := h.queue.counts(); len(requeues) != 0 {
		t.Errorf("no retry for a hopeless job: %+v", requeues)
	}
}

func TestWorkerRetriesTransientFailuresThenGivesUp(t *testing.T) {
	var calls atomic.Int32
	a := analyzerFunc(func(context.Context, string, []byte, ProgressFunc) (contract.Analysis, error) {
		calls.Add(1)
		return contract.Analysis{}, &AnalysisError{Message: "분석 서버에 연결하지 못했어요", Retryable: true}
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{}, job("k1"))
	h.start()

	// attempt 1 fails: handed back to run again after the retry delay, and still pending
	eventually(t, "first retry scheduled", func() bool { _, _, r := h.queue.counts(); return len(r) == 1 })
	_, _, r := h.queue.counts()
	if r[0].refund || !r[0].notBefore.Equal(testNow.Add(contract.RetryDelay(1))) {
		t.Errorf("first retry: %+v", r[0])
	}
	if h.status("k1") != contract.StatusPending {
		t.Error("a retryable failure leaves the contract pending")
	}
	time.Sleep(30 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("it must wait for the delay before trying again: %d runs", got)
	}

	// time passes: attempt 2 fails and is handed back with the longer delay
	t1 := testNow.Add(time.Hour)
	h.clock.set(t1)
	eventually(t, "second retry scheduled", func() bool { _, _, r := h.queue.counts(); return len(r) == 2 })
	if _, _, r := h.queue.counts(); !r[1].notBefore.Equal(t1.Add(contract.RetryDelay(2))) {
		t.Errorf("second retry: %+v", r[1])
	}

	// more time passes: attempt 3 is the last one, so the contract fails for good
	h.clock.set(t1.Add(time.Hour))
	eventually(t, "contract failed after the last attempt", func() bool { return h.status("k1") == contract.StatusFailed })
	if got := calls.Load(); got != contract.MaxAttempts {
		t.Errorf("analysis ran %d times, want %d", got, contract.MaxAttempts)
	}
	if c, _ := h.repo.status("k1"); c.ErrorMessage == nil || *c.ErrorMessage != "분석 서버에 연결하지 못했어요" {
		t.Errorf("the last error is shown: %v", c.ErrorMessage)
	}
	if _, _, r := h.queue.counts(); len(r) != 2 {
		t.Errorf("the last attempt is not handed back: %d requeues", len(r))
	}
}

func TestWorkerGivesBackARunningJobOnShutdownWithoutCountingIt(t *testing.T) {
	started := make(chan struct{})
	a := analyzerFunc(func(ctx context.Context, _ string, _ []byte, _ ProgressFunc) (contract.Analysis, error) {
		close(started)
		<-ctx.Done() // runs until it is cancelled
		return contract.Analysis{}, &AnalysisError{Message: "분석이 중단됐어요. 다시 올려 주세요", Err: ctx.Err()}
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{ShutdownWait: 50 * time.Millisecond}, job("k1"))
	stop := h.start()
	<-started

	stop() // the stop signal; the job does not finish within ShutdownWait

	_, _, r := h.queue.counts()
	if len(r) != 1 || !r[0].refund || r[0].id != "k1" {
		t.Fatalf("the job is handed back and the attempt is refunded: %+v", r)
	}
	if h.status("k1") != contract.StatusPending {
		t.Errorf("an interrupted job is not a failed job: %v", h.status("k1"))
	}
}

func TestWorkerLetsAShortJobFinishDuringShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	a := analyzerFunc(func(ctx context.Context, _ string, _ []byte, on ProgressFunc) (contract.Analysis, error) {
		close(started)
		<-release
		return sampleResult(), nil
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{ShutdownWait: 5 * time.Second}, job("k1"))
	stop := h.start()
	<-started

	go func() { time.Sleep(30 * time.Millisecond); close(release) }()
	stop()

	if h.status("k1") != contract.StatusCompleted {
		t.Errorf("a job that finishes within the wait is completed: %v", h.status("k1"))
	}
	if _, _, r := h.queue.counts(); len(r) != 0 {
		t.Errorf("nothing is handed back: %+v", r)
	}
}

func TestWorkerDropsAJobWhoseLeaseWasLost(t *testing.T) {
	started := make(chan struct{})
	a := analyzerFunc(func(ctx context.Context, _ string, _ []byte, _ ProgressFunc) (contract.Analysis, error) {
		close(started)
		<-ctx.Done()
		return contract.Analysis{}, &AnalysisError{Message: "분석이 중단됐어요. 다시 올려 주세요", Err: ctx.Err()}
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{HeartbeatInterval: 5 * time.Millisecond}, job("k1"))
	h.queue.setHeartbeatOK(false) // another worker took the job over, or the user deleted it
	h.start()
	<-started

	eventually(t, "analysis cancelled", func() bool { hb, _, _ := h.queue.counts(); return hb > 0 })
	time.Sleep(50 * time.Millisecond) // give a wrong implementation the time to act
	if h.status("k1") != contract.StatusPending {
		t.Errorf("the new owner decides the outcome; this worker must not touch it: %v", h.status("k1"))
	}
	if _, _, r := h.queue.counts(); len(r) != 0 {
		t.Errorf("and must not hand it back either: %+v", r)
	}
}

func TestWorkerKeepsTheLeaseAliveWhileWorking(t *testing.T) {
	gate := make(chan struct{})
	a := analyzerFunc(func(ctx context.Context, _ string, _ []byte, _ ProgressFunc) (contract.Analysis, error) {
		<-gate
		return sampleResult(), nil
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{HeartbeatInterval: 5 * time.Millisecond}, job("k1"))
	h.start()

	eventually(t, "several heartbeats while the job runs", func() bool { hb, _, _ := h.queue.counts(); return hb >= 3 })
	close(gate)
	eventually(t, "completed", func() bool { return h.status("k1") == contract.StatusCompleted })
}

func TestWorkerFailsAJobWhosePDFIsMissing(t *testing.T) {
	h := newHarness(t, analyzerFunc(okAnalyzer), fakeFiles{missing: map[string]bool{"k1": true}}, WorkerConfig{}, job("k1"))
	h.start()

	eventually(t, "failed", func() bool { return h.status("k1") == contract.StatusFailed })
	if c, _ := h.repo.status("k1"); c.ErrorMessage == nil || *c.ErrorMessage != contract.StaleMessage {
		t.Errorf("message: %v", c.ErrorMessage)
	}
}

func TestWorkerSurvivesAPanicInTheAnalyzer(t *testing.T) {
	a := analyzerFunc(func(context.Context, string, []byte, ProgressFunc) (contract.Analysis, error) { panic("boom") })
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{}, job("k1"), job("k2"))
	h.start()

	eventually(t, "both jobs failed, so the worker kept going after the panic", func() bool {
		return h.status("k1") == contract.StatusFailed && h.status("k2") == contract.StatusFailed
	})
}

func TestWorkerRunsJobsConcurrently(t *testing.T) {
	var running, peak atomic.Int32
	gate := make(chan struct{})
	a := analyzerFunc(func(ctx context.Context, _ string, _ []byte, _ ProgressFunc) (contract.Analysis, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-gate
		running.Add(-1)
		return sampleResult(), nil
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{Concurrency: 2}, job("k1"), job("k2"), job("k3"))
	h.start()

	eventually(t, "two jobs running at the same time", func() bool { return running.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if running.Load() != 2 {
		t.Errorf("Concurrency 2 means two at a time, not %d", running.Load())
	}
	close(gate)
	eventually(t, "all three completed", func() bool {
		return h.status("k1") == contract.StatusCompleted && h.status("k2") == contract.StatusCompleted && h.status("k3") == contract.StatusCompleted
	})
	if peak.Load() != 2 {
		t.Errorf("peak concurrency = %d", peak.Load())
	}
}

func TestWorkerRunsTheReaperRegularly(t *testing.T) {
	h := newHarness(t, analyzerFunc(okAnalyzer), fakeFiles{}, WorkerConfig{ReapInterval: 10 * time.Millisecond})
	h.start()
	eventually(t, "the queue is reaped more than once", func() bool { _, reaps, _ := h.queue.counts(); return reaps >= 3 })
}

func TestWorkerDiscardsTheResultOfADeletedContract(t *testing.T) {
	// the contract is not in the repository (deleted): Complete returns false and the worker just moves on
	repo := newFakeRepo()
	q := newFakeQueue(job("gone"), job("k2"))
	repo.Create(context.Background(), contract.Contract{ID: "k2", UserID: "u1", Status: contract.StatusPending})
	w := NewWorker(q, fakeFiles{}, repo, analyzerFunc(okAnalyzer), &clock.Fixed{T: testNow},
		slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{PollInterval: 5 * time.Millisecond, ReapInterval: time.Hour, HeartbeatInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	eventually(t, "the next job is still processed", func() bool { c, _ := repo.status("k2"); return c.Status == contract.StatusCompleted })
	if _, ok := repo.status("gone"); ok {
		t.Error("the deleted contract must not come back")
	}
}

func TestWorkerErrorFromProgressUpdateIsRetried(t *testing.T) {
	boom := errors.New("db down")
	a := analyzerFunc(func(context.Context, string, []byte, ProgressFunc) (contract.Analysis, error) {
		return contract.Analysis{}, boom
	})
	h := newHarness(t, a, fakeFiles{}, WorkerConfig{}, job("k1"))
	h.start()

	eventually(t, "handed back for a retry", func() bool { _, _, r := h.queue.counts(); return len(r) == 1 })
	if h.status("k1") != contract.StatusPending {
		t.Error("a transient error is not final")
	}
}
