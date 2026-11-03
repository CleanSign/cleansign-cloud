package contract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
)

// fakeRepo is a Repository that stores in memory.
type fakeRepo struct {
	mu        sync.Mutex
	contracts map[string]contract.Contract
	clauses   map[string][]contract.Clause
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{contracts: map[string]contract.Contract{}, clauses: map[string][]contract.Clause{}}
}

func (r *fakeRepo) Create(_ context.Context, c contract.Contract) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contracts[c.ID] = c
	return nil
}

func (r *fakeRepo) Get(_ context.Context, userID, id string) (contract.Contract, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contracts[id]
	if !ok || c.UserID != userID {
		return contract.Contract{}, errs.ErrNotFound
	}
	return c, nil
}

func (r *fakeRepo) Clauses(_ context.Context, id string) ([]contract.Clause, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clauses[id], nil
}

func (r *fakeRepo) Page(context.Context, string, int, int) (contract.Page, error) {
	return contract.Page{}, nil
}
func (r *fakeRepo) Recent(context.Context, string, int) ([]contract.Summary, error) { return nil, nil }

func (r *fakeRepo) Delete(_ context.Context, userID, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contracts[id]
	if !ok || c.UserID != userID {
		return false, nil
	}
	delete(r.contracts, id)
	delete(r.clauses, id)
	return true, nil
}

func (r *fakeRepo) UpdateProgress(_ context.Context, id string, p contract.Progress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contracts[id]; ok {
		c.Progress = &p
		r.contracts[id] = c
	}
	return nil
}

func (r *fakeRepo) Complete(_ context.Context, id string, res contract.Result, cl []contract.Clause) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contracts[id]
	if !ok {
		return false, nil
	}
	c.Status = contract.StatusCompleted
	c.LossAmount = res.LossAmount
	c.Progress = nil
	r.contracts[id] = c
	r.clauses[id] = cl
	return true, nil
}

func (r *fakeRepo) Fail(_ context.Context, id, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.contracts[id]; ok {
		c.Status = contract.StatusFailed
		c.ErrorMessage = &message
		c.Progress = nil
		r.contracts[id] = c
	}
	return nil
}

func (r *fakeRepo) status(id string) (contract.Contract, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.contracts[id]
	return c, ok
}

func sampleResult() contract.Analysis {
	return contract.Analysis{
		RiskScore: 70, Grade: contract.SeverityHigh, LossAmount: 1000, LossMin: 500, LossMax: 1500,
		ModelVersion: "m", PolicyVersion: "p",
		Clauses: []contract.AnalysisClause{
			{Original: "a", ClauseType: "t", Severity: contract.SeverityHigh, ExpectedLoss: 400, Reason: "r", SafeText: "s"},
			{Original: "b", ClauseType: "t", Severity: contract.SeverityLow, ExpectedLoss: 100, Reason: "r", SafeText: "s"},
		},
	}
}

func newService(repo *fakeRepo) (Service, *fakeQueue, *clock.Fixed) {
	clk := &clock.Fixed{T: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	q := newFakeQueue()
	q.repo = repo // the real Enqueue creates the contract; the fake does the same
	return NewService(repo, q, clk), q, clk
}

func submit(t *testing.T, s Service, user string) string {
	t.Helper()
	id, err := s.Submit(context.Background(), user, "a.pdf", "application/pdf", 4, strings.NewReader("%PDF"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return id
}

// complete saves the sample analysis for a queued contract, as a worker would.
func complete(t *testing.T, repo *fakeRepo, id string) {
	t.Helper()
	a := sampleResult()
	clauses := contract.ClausesFrom(id, a)
	for i := range clauses {
		clauses[i].ID = "c" + string(rune('0'+i))
	}
	if saved, err := repo.Complete(context.Background(), id, contract.ResultFrom(a), clauses); err != nil || !saved {
		t.Fatalf("Complete: %v %v", saved, err)
	}
}

func TestSubmitValidatesUpload(t *testing.T) {
	s, q, _ := newService(newFakeRepo())
	ctx := context.Background()
	if _, err := s.Submit(ctx, "u1", "a.txt", "text/plain", 4, strings.NewReader("x")); !errors.Is(err, contract.ErrNotPDF) {
		t.Errorf("PDF가 아니면 ErrNotPDF: %v", err)
	}
	if _, err := s.Submit(ctx, "u1", "a.pdf", "application/pdf", 0, strings.NewReader("")); !errors.Is(err, contract.ErrBadSize) {
		t.Errorf("빈 파일은 ErrBadSize: %v", err)
	}
	big := bytes.NewReader(make([]byte, contract.MaxUploadBytes+1))
	if _, err := s.Submit(ctx, "u1", "a.pdf", "application/pdf", 1, big); !errors.Is(err, contract.ErrBadSize) {
		t.Errorf("선언한 크기와 달리 실제로 크면 ErrBadSize: %v", err)
	}
	if n := len(q.enqueued()); n != 0 {
		t.Errorf("거절된 업로드는 큐에 들어가지 않는다: %d", n)
	}
}

func TestSubmitQueuesTheContractWithItsPDF(t *testing.T) {
	repo := newFakeRepo()
	s, q, clk := newService(repo)

	id := submit(t, s, "u1")

	got := q.enqueued()
	if len(got) != 1 {
		t.Fatalf("한 건이 큐에 들어가야 한다: %d", len(got))
	}
	c := got[0].contract
	if c.ID != id || c.UserID != "u1" || c.FileName != "a.pdf" || c.MimeType != "application/pdf" || !c.CreatedAt.Equal(clk.Now()) {
		t.Errorf("큐에 들어간 계약서: %+v", c)
	}
	if c.Status != contract.StatusPending || c.Progress == nil || c.Progress.Stage != "reading" {
		t.Errorf("처음 상태는 pending, 단계는 reading: %+v", c)
	}
	if string(got[0].pdf) != "%PDF" {
		t.Errorf("PDF 내용이 함께 들어가야 한다: %q", got[0].pdf)
	}
	// the response does not wait for any analysis: right after Submit the contract is simply pending
	if stored, ok := repo.status(id); !ok || stored.Status != contract.StatusPending {
		t.Errorf("제출 직후 상태: %+v", stored)
	}
}

func TestSubmitReturnsTheQueueError(t *testing.T) {
	repo := newFakeRepo()
	s, q, _ := newService(repo)
	boom := errors.New("db down")
	q.enqueueErr = boom

	if _, err := s.Submit(context.Background(), "u1", "a.pdf", "application/pdf", 4, strings.NewReader("%PDF")); !errors.Is(err, boom) {
		t.Fatalf("큐에 넣지 못하면 그 오류를 돌려준다: %v", err)
	}
	if len(repo.contracts) != 0 {
		t.Error("계약서가 남으면 안 된다")
	}
}

func TestGetOnlyOwnContract(t *testing.T) {
	repo := newFakeRepo()
	s, _, _ := newService(repo)
	id := submit(t, s, "u1")
	complete(t, repo, id)

	if _, _, err := s.Get(context.Background(), "u2", id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("남의 계약서는 NotFound: %v", err)
	}
	if err := s.Delete(context.Background(), "u2", id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("남의 계약서 삭제는 NotFound: %v", err)
	}
	if err := s.Delete(context.Background(), "u1", id); err != nil {
		t.Errorf("내 계약서는 지울 수 있다: %v", err)
	}
}

func TestGetShowsClausesOnlyWhenCompleted(t *testing.T) {
	repo := newFakeRepo()
	s, _, clk := newService(repo)
	id := submit(t, s, "u1")

	// a waiting contract stays pending however long it waits: what happens to it is up to the queue, not to a read
	clk.Advance(48 * time.Hour)
	c, clauses, err := s.Get(context.Background(), "u1", id)
	if err != nil || c.Status != contract.StatusPending || clauses != nil {
		t.Fatalf("대기 중: %+v %v %v", c, clauses, err)
	}

	complete(t, repo, id)
	c, clauses, err = s.Get(context.Background(), "u1", id)
	if err != nil || c.Status != contract.StatusCompleted || len(clauses) != 2 {
		t.Fatalf("완료: %+v %v %v", c, clauses, err)
	}
}

func TestClause(t *testing.T) {
	repo := newFakeRepo()
	s, _, _ := newService(repo)
	id := submit(t, s, "u1")
	complete(t, repo, id)

	_, clauses, _ := s.Get(context.Background(), "u1", id)
	got, err := s.Clause(context.Background(), "u1", id, clauses[1].ID)
	if err != nil || got.Original != "b" {
		t.Errorf("조항을 찾아야 한다: %+v %v", got, err)
	}
	if _, err := s.Clause(context.Background(), "u1", id, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("없는 조항은 NotFound: %v", err)
	}
	if _, err := s.Clause(context.Background(), "u2", id, clauses[0].ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("남의 계약서의 조항은 NotFound: %v", err)
	}
}

func TestClauseRequiresCompleted(t *testing.T) {
	repo := newFakeRepo()
	s, _, _ := newService(repo)
	id := submit(t, s, "u1")
	if _, err := s.Clause(context.Background(), "u1", id, "any"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("완료되지 않은 계약서의 조항은 NotFound: %v", err)
	}
}
