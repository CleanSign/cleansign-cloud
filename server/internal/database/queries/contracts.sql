-- name: CreateContract :exec
INSERT INTO contract (id, user_id, file_name, mime_type, status, progress)
VALUES ($1, $2, $3, $4, 'pending', $5);

-- name: GetContractForUser :one
SELECT * FROM contract WHERE id = $1 AND user_id = $2;

-- name: GetContractByID :one
SELECT * FROM contract WHERE id = $1;

-- name: ListClausesByContract :many
SELECT * FROM contract_danger_clause WHERE contract_id = $1 ORDER BY order_index;

-- name: ListContractsPage :many
SELECT c.id, c.file_name, c.created_at, c.status, c.error_message, c.loss_amount,
       (SELECT count(*) FROM contract_danger_clause d WHERE d.contract_id = c.id)::int AS clause_count
FROM contract c
WHERE c.user_id = $1
ORDER BY c.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountContracts :one
SELECT count(*)::int FROM contract WHERE user_id = $1;

-- name: ListRecentContracts :many
SELECT c.id, c.file_name, c.created_at, c.status, c.loss_amount,
       (SELECT count(*) FROM contract_danger_clause d WHERE d.contract_id = c.id)::int AS clause_count
FROM contract c
WHERE c.user_id = $1 AND c.status IN ('completed', 'pending')
ORDER BY c.created_at DESC
LIMIT $2;

-- name: DeleteContractForUser :execrows
DELETE FROM contract WHERE id = $1 AND user_id = $2;

-- name: UpdateContractProgress :exec
UPDATE contract SET progress = $2 WHERE id = $1;

-- name: CompleteContract :execrows
UPDATE contract
SET status = 'completed',
    loss_amount = $2,
    risk_score = $3,
    grade = $4,
    loss_min = $5,
    loss_max = $6,
    deposit_amount = $7,
    model_version = $8,
    warnings = $9,
    risk_flow = $10,
    progress = NULL,
    error_message = NULL,
    lease_until = NULL,
    locked_by = NULL
-- Only a job that is still pending can complete. A second worker that finishes after another one already did is ignored.
WHERE id = $1 AND status = 'pending';

-- name: InsertClause :exec
INSERT INTO contract_danger_clause
    (id, contract_id, original, clause_type, confidence, l_max, loss_amount, severity, reason, law_reference, safe_text, order_index)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: FailContract :execrows
UPDATE contract
SET status = 'failed', error_message = $2, progress = NULL, lease_until = NULL, locked_by = NULL
WHERE id = $1 AND status = 'pending';
