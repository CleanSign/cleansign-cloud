package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CleanSign/cleansign/server/internal/database/sqlc"
	"github.com/CleanSign/cleansign/server/internal/domain/contract"
)

// lawReferenceSep is the separator used to join multiple laws into one column. It matches the existing data format.
const lawReferenceSep = ", "

// ContractRepo implements the contract port.
type ContractRepo struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewContractRepo(pool *pgxpool.Pool) *ContractRepo {
	return &ContractRepo{pool: pool, q: queries(pool)}
}

func (r *ContractRepo) Create(ctx context.Context, c contract.Contract) error {
	progress, err := marshalProgress(c.Progress)
	if err != nil {
		return err
	}
	return r.q.CreateContract(ctx, sqlc.CreateContractParams{
		ID: c.ID, UserID: c.UserID, FileName: c.FileName, MimeType: c.MimeType, Progress: progress,
	})
}

func (r *ContractRepo) Get(ctx context.Context, userID, id string) (contract.Contract, error) {
	row, err := r.q.GetContractForUser(ctx, sqlc.GetContractForUserParams{ID: id, UserID: userID})
	if err != nil {
		return contract.Contract{}, notFound(err)
	}
	return toContract(row)
}

func (r *ContractRepo) Clauses(ctx context.Context, contractID string) ([]contract.Clause, error) {
	rows, err := r.q.ListClausesByContract(ctx, contractID)
	if err != nil {
		return nil, err
	}
	out := make([]contract.Clause, len(rows))
	for i, row := range rows {
		out[i] = toClause(row)
	}
	return out, nil
}

func (r *ContractRepo) Page(ctx context.Context, userID string, page, size int) (contract.Page, error) {
	rows, err := r.q.ListContractsPage(ctx, sqlc.ListContractsPageParams{UserID: userID, Limit: int32(size), Offset: int32((page - 1) * size)})
	if err != nil {
		return contract.Page{}, err
	}
	total, err := r.q.CountContracts(ctx, userID)
	if err != nil {
		return contract.Page{}, err
	}
	items := make([]contract.Summary, len(rows))
	for i, row := range rows {
		items[i] = contract.Summary{
			ID: row.ID, FileName: row.FileName, CreatedAt: row.CreatedAt.UTC(), Status: contract.Status(row.Status),
			ErrorMessage: row.ErrorMessage, LossAmount: row.LossAmount, ClauseCount: row.ClauseCount,
		}
	}
	return contract.Page{Items: items, Page: page, Size: size, Total: int(total), HasMore: page*size < int(total)}, nil
}

func (r *ContractRepo) Recent(ctx context.Context, userID string, limit int) ([]contract.Summary, error) {
	rows, err := r.q.ListRecentContracts(ctx, sqlc.ListRecentContractsParams{UserID: userID, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	items := make([]contract.Summary, len(rows))
	for i, row := range rows {
		items[i] = contract.Summary{
			ID: row.ID, FileName: row.FileName, CreatedAt: row.CreatedAt.UTC(), Status: contract.Status(row.Status),
			LossAmount: row.LossAmount, ClauseCount: row.ClauseCount,
		}
	}
	return items, nil
}

func (r *ContractRepo) Delete(ctx context.Context, userID, id string) (bool, error) {
	n, err := r.q.DeleteContractForUser(ctx, sqlc.DeleteContractForUserParams{ID: id, UserID: userID})
	return n > 0, err
}

func (r *ContractRepo) UpdateProgress(ctx context.Context, id string, p contract.Progress) error {
	b, err := marshalProgress(&p)
	if err != nil {
		return err
	}
	return r.q.UpdateContractProgress(ctx, sqlc.UpdateContractProgressParams{ID: id, Progress: b})
}

// Complete saves the result and clauses in one transaction. It returns false if the contract has already been deleted.
func (r *ContractRepo) Complete(ctx context.Context, id string, res contract.Result, clauses []contract.Clause) (bool, error) {
	warnings, err := json.Marshal(res.Warnings)
	if err != nil {
		return false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is ignored

	q := r.q.WithTx(tx)
	grade := sqlc.ClauseSeverity(res.Grade)
	score := res.RiskScore
	lossMin, lossMax := res.LossMin, res.LossMax
	model := res.ModelVersion
	updated, err := q.CompleteContract(ctx, sqlc.CompleteContractParams{
		ID: id, LossAmount: res.LossAmount, RiskScore: &score, Grade: &grade,
		LossMin: &lossMin, LossMax: &lossMax, DepositAmount: res.DepositAmount,
		ModelVersion: &model, Warnings: warnings, RiskFlow: nilIfJSONNull(res.RiskFlow),
	})
	if err != nil {
		return false, err
	}
	if updated == 0 {
		// deleted during analysis, or already finished by another worker: discard this result
		return false, nil
	}
	for _, c := range clauses {
		if err := q.InsertClause(ctx, insertClauseParams(c)); err != nil {
			return false, err
		}
	}
	// the PDF is no longer needed once the result is saved
	if err := q.DeleteContractFile(ctx, id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Fail marks a pending contract failed and deletes its PDF, in one transaction.
func (r *ContractRepo) Fail(ctx context.Context, id, message string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is ignored

	q := r.q.WithTx(tx)
	if _, err := q.FailContract(ctx, sqlc.FailContractParams{ID: id, ErrorMessage: &message}); err != nil {
		return err
	}
	if err := q.DeleteContractFile(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- Conversion ---

func toContract(r sqlc.Contract) (contract.Contract, error) {
	c := contract.Contract{
		ID: r.ID, UserID: r.UserID, FileName: r.FileName, MimeType: r.MimeType,
		Status: contract.Status(r.Status), LossAmount: r.LossAmount, RiskScore: r.RiskScore,
		LossMin: r.LossMin, LossMax: r.LossMax, DepositAmount: r.DepositAmount, ModelVersion: r.ModelVersion,
		ErrorMessage: r.ErrorMessage, CreatedAt: r.CreatedAt.UTC(),
	}
	if r.Grade != nil {
		g := contract.Severity(*r.Grade)
		c.Grade = &g
	}
	if len(r.Warnings) > 0 {
		if err := json.Unmarshal(r.Warnings, &c.Warnings); err != nil {
			return contract.Contract{}, fmt.Errorf("decode warnings: %w", err)
		}
	}
	c.RiskFlow = nilIfJSONNull(r.RiskFlow)
	if len(r.Progress) > 0 {
		var p *contract.Progress
		if err := json.Unmarshal(r.Progress, &p); err != nil {
			return contract.Contract{}, fmt.Errorf("decode progress: %w", err)
		}
		c.Progress = p
	}
	return c, nil
}

func toClause(r sqlc.ContractDangerClause) contract.Clause {
	c := contract.Clause{
		ID: r.ID, ContractID: r.ContractID, Original: r.Original, ClauseType: r.ClauseType,
		Confidence: r.Confidence, LMax: r.LMax, LossAmount: r.LossAmount, Severity: contract.Severity(r.Severity),
		Reason: r.Reason, SafeText: r.SafeText, OrderIndex: r.OrderIndex,
	}
	if r.LawReference != nil && *r.LawReference != "" {
		c.LawReference = strings.Split(*r.LawReference, lawReferenceSep)
	}
	return c
}

func insertClauseParams(c contract.Clause) sqlc.InsertClauseParams {
	var law *string
	if joined := strings.Join(c.LawReference, lawReferenceSep); joined != "" {
		law = &joined
	}
	return sqlc.InsertClauseParams{
		ID: c.ID, ContractID: c.ContractID, Original: c.Original, ClauseType: c.ClauseType,
		Confidence: c.Confidence, LMax: c.LMax, LossAmount: c.LossAmount, Severity: sqlc.ClauseSeverity(c.Severity),
		Reason: c.Reason, LawReference: law, SafeText: c.SafeText, OrderIndex: c.OrderIndex,
	}
}

func marshalProgress(p *contract.Progress) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	return json.Marshal(p)
}

// nilIfJSONNull returns nil for empty or JSON null values (the column is stored as NULL).
func nilIfJSONNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}
