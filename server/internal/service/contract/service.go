// Package contract assembles the flow of contract upload, background analysis, and lookup/deletion.
package contract

import (
	"context"
	"fmt"
	"io"

	"github.com/CleanSign/cleansign/server/internal/domain/contract"
	"github.com/CleanSign/cleansign/server/internal/shared/clock"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
	"github.com/CleanSign/cleansign/server/internal/shared/ids"
)

const (
	pdfMime       = "application/pdf"
	saveFailedMsg = "분석을 저장하는 중 문제가 생겼어요"
)

// 생성 패키지 이름을 임시로 다르게 주는 이유: domain/contract와 이름이 같으면 생성기가 `contract.` 접두사를 지워 버린다.
//
//go:generate go tool ifacemaker -f service.go -s service -i Service -p contractgen -o service_interface.go -y "Service는 contract 흐름의 공개 메서드다. handler가 이 인터페이스에 의존하고, 테스트에서는 mock으로 바꾼다."
//go:generate perl -pi -e "s/^package contractgen/package contract/" service_interface.go
type service struct {
	repo  Repository
	queue Queue
	clock clock.Clock
}

func NewService(repo Repository, queue Queue, clk clock.Clock) Service {
	return &service{repo: repo, queue: queue, clock: clk}
}

// Submit validates the upload and puts it in the analysis queue together with the PDF. It does not wait for the analysis:
// a worker (in this process or another) picks the job up.
func (s *service) Submit(ctx context.Context, userID, fileName, contentType string, size int64, body io.Reader) (string, error) {
	if err := contract.ValidateUpload(fileName, contentType, size); err != nil {
		return "", err
	}
	// the declared size is not trusted: read at most one byte more than allowed
	data, err := io.ReadAll(io.LimitReader(body, contract.MaxUploadBytes+1))
	if err != nil {
		return "", fmt.Errorf("read upload: %w", err)
	}
	if err := contract.ValidateUpload(fileName, contentType, int64(len(data))); err != nil {
		return "", err
	}

	progress := contract.InitialProgress()
	c := contract.Contract{
		ID:        ids.New(),
		UserID:    userID,
		FileName:  fileName,
		MimeType:  pdfMime,
		Status:    contract.StatusPending,
		Progress:  &progress,
		CreatedAt: s.clock.Now(),
	}
	if err := s.queue.Enqueue(ctx, c, data); err != nil {
		return "", err
	}
	return c.ID, nil
}

// Get returns the caller's own contract. Clauses are included only when it is completed.
func (s *service) Get(ctx context.Context, userID, id string) (contract.Contract, []contract.Clause, error) {
	c, err := s.repo.Get(ctx, userID, id)
	if err != nil {
		return contract.Contract{}, nil, err
	}
	if c.Status != contract.StatusCompleted {
		return c, nil, nil
	}
	clauses, err := s.repo.Clauses(ctx, id)
	if err != nil {
		return contract.Contract{}, nil, err
	}
	return c, clauses, nil
}

// Clause returns one clause of a completed contract.
func (s *service) Clause(ctx context.Context, userID, contractID, clauseID string) (contract.Clause, error) {
	c, clauses, err := s.Get(ctx, userID, contractID)
	if err != nil {
		return contract.Clause{}, err
	}
	if c.Status != contract.StatusCompleted {
		return contract.Clause{}, errs.ErrNotFound
	}
	for _, cl := range clauses {
		if cl.ID == clauseID {
			return cl, nil
		}
	}
	return contract.Clause{}, errs.ErrNotFound
}

// List returns one page of history (newest first, including failed analyses).
func (s *service) List(ctx context.Context, userID string, page, size int) (contract.Page, error) {
	return s.repo.Page(ctx, userID, page, size)
}

// Recent returns up to limit completed and in-progress analyses, newest first (failed excluded).
func (s *service) Recent(ctx context.Context, userID string, limit int) ([]contract.Summary, error) {
	return s.repo.Recent(ctx, userID, limit)
}

// Delete deletes the caller's contract and its analysis results.
func (s *service) Delete(ctx context.Context, userID, id string) error {
	deleted, err := s.repo.Delete(ctx, userID, id)
	if err != nil {
		return err
	}
	if !deleted {
		return errs.ErrNotFound
	}
	return nil
}
