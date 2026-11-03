-- The analysis queue. See migration 00003 for the model: a pending contract with lease_until NULL and next_run_at <= now is runnable.

-- name: InsertContractFile :exec
INSERT INTO contract_file (contract_id, data) VALUES (@contract_id, @data);

-- name: GetContractFile :one
SELECT data FROM contract_file WHERE contract_id = @contract_id;

-- name: DeleteContractFile :exec
DELETE FROM contract_file WHERE contract_id = @contract_id;

-- name: ClaimContract :one
-- Takes the oldest runnable job. SKIP LOCKED lets many workers claim at the same time without waiting for each other.
-- A job that waited longer than the queue allows (created before @not_before) is never claimed: the reaper fails it,
-- so an old upload is not analysed (and its PDF kept) long after the user gave up on it.
UPDATE contract
SET attempts = attempts + 1, lease_until = @lease_until, locked_by = @worker
WHERE id = (
    SELECT c.id FROM contract c
    WHERE c.status = 'pending' AND c.lease_until IS NULL AND c.next_run_at <= @now AND c.attempts < @max_attempts::int
      AND c.created_at > @not_before
    ORDER BY c.next_run_at, c.created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, user_id, file_name, attempts;

-- name: HeartbeatContract :execrows
-- Extends the lease. Zero rows means the caller no longer owns the job (it was reaped, taken over, finished or deleted).
UPDATE contract SET lease_until = @lease_until
WHERE id = @id AND locked_by = @worker AND status = 'pending' AND lease_until IS NOT NULL;

-- name: RequeueContract :execrows
-- The owner gives the job back. refund = 1 takes back the attempt that was just counted (used when a shutdown cancelled it).
UPDATE contract
SET lease_until = NULL, locked_by = NULL, next_run_at = @next_run_at,
    attempts = GREATEST(attempts - @refund::int, 0), progress = @progress
WHERE id = @id AND locked_by = @worker AND status = 'pending';

-- name: ListExpiredLeases :many
SELECT id, attempts FROM contract
WHERE status = 'pending' AND lease_until IS NOT NULL AND lease_until < @now AND attempts < @max_attempts::int
ORDER BY lease_until
LIMIT @batch;

-- name: RequeueExpiredLease :execrows
UPDATE contract
SET lease_until = NULL, locked_by = NULL, next_run_at = @next_run_at, progress = @progress
WHERE id = @id AND status = 'pending' AND lease_until IS NOT NULL AND lease_until < @now;

-- name: FailExhaustedLeases :execrows
-- The worker disappeared on its last allowed attempt: give up.
UPDATE contract
SET status = 'failed', error_message = @message, progress = NULL, lease_until = NULL, locked_by = NULL
WHERE status = 'pending' AND lease_until IS NOT NULL AND lease_until < @now AND attempts >= @max_attempts::int;

-- name: FailExpiredQueued :execrows
-- Waiting in the queue for too long without being claimed.
UPDATE contract
SET status = 'failed', error_message = @message, progress = NULL
WHERE status = 'pending' AND lease_until IS NULL AND created_at < @created_before;

-- name: DeleteFinishedContractFiles :execrows
-- Safety net: no PDF may outlive its analysis, whichever way the contract ended.
DELETE FROM contract_file f USING contract c WHERE f.contract_id = c.id AND c.status <> 'pending';
