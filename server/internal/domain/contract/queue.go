package contract

import "time"

// Rules of the analysis queue.
const (
	// MaxAttempts is how many times a job is tried in total (the first run included).
	MaxAttempts = 3

	// LeaseDuration is how long a worker owns a job without renewing it. If the worker dies, the job becomes retryable after this.
	LeaseDuration = 90 * time.Second

	// HeartbeatInterval is how often a running worker renews its lease. It must be well below LeaseDuration.
	HeartbeatInterval = 30 * time.Second

	// QueueExpiry: a contract still waiting in the queue after this is cancelled (nobody picked it up).
	QueueExpiry = 24 * time.Hour

	// QueueExpiredMessage is shown when a contract waited in the queue for too long.
	QueueExpiredMessage = "분석 대기 시간이 너무 길어져 취소됐어요. 다시 올려 주세요"
)

// RetryDelay is how long to wait before the next attempt, given how many attempts have been made so far.
// After the 1st attempt: 30 seconds. After the 2nd or later: 2 minutes.
func RetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return 30 * time.Second
	}
	return 2 * time.Minute
}

// CanRetry reports whether another attempt is allowed after attempts have been made.
func CanRetry(attempts int) bool { return attempts < MaxAttempts }

// Job is a claimed analysis job.
type Job struct {
	ID       string
	UserID   string
	FileName string
	// Attempts includes the current attempt.
	Attempts int
}

// ReapResult is what one reaper pass did.
type ReapResult struct {
	// Requeued: jobs whose worker disappeared and that were put back in the queue.
	Requeued int
	// Failed: jobs given up on (attempts used up, or waited in the queue too long).
	Failed int
}
