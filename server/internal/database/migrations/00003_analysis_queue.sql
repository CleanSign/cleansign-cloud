-- +goose Up
-- The contract table doubles as the analysis queue. A pending contract is runnable when lease_until is NULL and
-- next_run_at has passed. A worker claims it by setting lease_until (and keeps extending it while it works);
-- if the worker dies the lease expires and the job is retried.
ALTER TABLE contract
    ADD COLUMN attempts     integer     NOT NULL DEFAULT 0,
    ADD COLUMN next_run_at  timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN lease_until  timestamptz,
    -- worker id, for diagnostics and so that only the owner can extend or release the lease
    ADD COLUMN locked_by    text;

CREATE INDEX contract_queue_idx ON contract (next_run_at) WHERE status = 'pending' AND lease_until IS NULL;

-- The uploaded PDF, kept only while the analysis is not finished (it is deleted when the contract completes or fails).
-- It lives in its own table so that `SELECT * FROM contract` never drags up to 10MB along.
-- contract.file_key stays NULL for now: it is reserved for the key of an object storage implementation.
CREATE TABLE contract_file (
    contract_id text PRIMARY KEY REFERENCES contract (id) ON DELETE CASCADE,
    data        bytea       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE contract_file;
DROP INDEX contract_queue_idx;
ALTER TABLE contract
    DROP COLUMN locked_by,
    DROP COLUMN lease_until,
    DROP COLUMN next_run_at,
    DROP COLUMN attempts;
