package daemon

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"golang.org/x/time/rate"

	ghpkg "go.kenn.io/roborev/internal/github"
	reviewpkg "go.kenn.io/roborev/internal/review"
)

// Line comments are posted from one queue per daemon, one request at a time,
// because GitHub asks integrations to send writes serially, at least a second
// apart, and limits content creation to 80 requests a minute and 500 an hour.
// GitHub does not say how a review's comments count against that limit, so
// the queue budgets each comment as one item.
const (
	inlineQueueSize          = 100
	inlineMinWriteGap        = time.Second
	inlineCommentsPerMinute  = 30
	inlineCommentBurst       = ghpkg.InlineBatchSize * 2
	inlineMaxAttempts        = 3
	inlineDefaultRetryDelay  = time.Minute
	inlineMaxRetryDelay      = 15 * time.Minute
	inlineRequestTimeout     = 60 * time.Second
	inlineEnqueueDropMessage = "CI poller: line comment queue full, dropping line comments for %s#%d"
)

// inlineReviewJob is one head commit's line comments.
type inlineReviewJob struct {
	repo     string
	pr       int
	headSHA  string
	summary  string
	findings []reviewpkg.InlineFinding
}

// inlineReviewClient is the part of the GitHub client the queue uses.
type inlineReviewClient interface {
	PlanInlineReview(ctx context.Context, ghRepo string, prNumber int, headSHA string, findings []reviewpkg.InlineFinding) ([]ghpkg.InlineBatch, error)
	PostInlineBatch(ctx context.Context, ghRepo string, prNumber int, headSHA, summary string, batch ghpkg.InlineBatch) error
}

// inlineReviewQueue posts line comments in the background. It starts its
// worker on the first job and drops pending work on shutdown; the PR comment
// already carries every finding, so dropped line comments lose nothing.
type inlineReviewQueue struct {
	client  func(ghRepo string) (inlineReviewClient, error)
	limiter *rate.Limiter
	sleep   func(ctx context.Context, d time.Duration) error
	now     func() time.Time

	mu        sync.Mutex
	started   bool
	jobs      chan inlineReviewJob
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	lastWrite time.Time
}

func newInlineReviewQueue(client func(ghRepo string) (inlineReviewClient, error)) *inlineReviewQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &inlineReviewQueue{
		client:  client,
		limiter: rate.NewLimiter(rate.Every(time.Minute/inlineCommentsPerMinute), inlineCommentBurst),
		sleep:   sleepContext,
		now:     time.Now,
		jobs:    make(chan inlineReviewJob, inlineQueueSize),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
}

// enqueue adds a job without blocking the caller.
func (q *inlineReviewQueue) enqueue(job inlineReviewJob) {
	q.mu.Lock()
	if !q.started {
		q.started = true
		go q.run()
	}
	q.mu.Unlock()
	select {
	case q.jobs <- job:
	default:
		log.Printf(inlineEnqueueDropMessage, job.repo, job.pr)
	}
}

// shutdown cancels pending and in-flight work and waits for the worker.
func (q *inlineReviewQueue) shutdown() {
	q.cancel()
	q.mu.Lock()
	started := q.started
	// Marking it started keeps a later enqueue from starting a worker.
	q.started = true
	q.mu.Unlock()
	if started {
		<-q.done
	}
}

func (q *inlineReviewQueue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			q.process(job)
		}
	}
}

// process posts one job's batches, retrying rate limits and transient
// failures up to inlineMaxAttempts times in total.
func (q *inlineReviewQueue) process(job inlineReviewJob) {
	client, err := q.client(job.repo)
	if err != nil {
		log.Printf("CI poller: line comments for %s#%d: %v", job.repo, job.pr, err)
		return
	}
	posted := 0
	for attempt := 1; ; attempt++ {
		batches, err := q.plan(client, job)
		if err == nil {
			var n int
			n, err = q.postBatches(client, job, batches)
			posted += n
			if err == nil {
				if posted > 0 {
					log.Printf("CI poller: posted %d line comments on %s#%d", posted, job.repo, job.pr)
				}
				return
			}
		}
		if errors.Is(err, ghpkg.ErrInlineHeadMoved) {
			log.Printf("CI poller: skipping line comments for %s#%d: %v", job.repo, job.pr, err)
			return
		}
		if q.ctx.Err() != nil {
			return
		}
		if attempt == inlineMaxAttempts {
			log.Printf("CI poller: giving up on line comments for %s#%d after %d attempts: %v",
				job.repo, job.pr, attempt, err)
			return
		}
		delay := retryDelay(err, attempt)
		log.Printf("CI poller: line comments for %s#%d failed (attempt %d), retrying in %s: %v",
			job.repo, job.pr, attempt, delay, err)
		if q.sleep(q.ctx, delay) != nil {
			return
		}
	}
}

func (q *inlineReviewQueue) plan(client inlineReviewClient, job inlineReviewJob) ([]ghpkg.InlineBatch, error) {
	ctx, cancel := context.WithTimeout(q.ctx, inlineRequestTimeout)
	defer cancel()
	return client.PlanInlineReview(ctx, job.repo, job.pr, job.headSHA, job.findings)
}

// postBatches posts batches in order and returns how many comments GitHub
// accepted. A batch GitHub rejects as invalid is retried once with single-line
// comments and then skipped; any other error stops so the caller can retry,
// and the next plan leaves out the batches already posted.
func (q *inlineReviewQueue) postBatches(client inlineReviewClient, job inlineReviewJob, batches []ghpkg.InlineBatch) (int, error) {
	posted := 0
	for _, batch := range batches {
		err := q.post(client, job, batch)
		if ghpkg.IsInlineValidationError(err) {
			err = q.post(client, job, batch.SingleLine())
			if ghpkg.IsInlineValidationError(err) {
				log.Printf("CI poller: GitHub rejected line comment batch %d for %s#%d: %v",
					batch.Index, job.repo, job.pr, err)
				continue
			}
		}
		if err != nil {
			return posted, err
		}
		posted += len(batch.Comments)
	}
	return posted, nil
}

// post sends one batch after waiting for the comment budget and the minimum
// gap since the previous write.
func (q *inlineReviewQueue) post(client inlineReviewClient, job inlineReviewJob, batch ghpkg.InlineBatch) error {
	if err := q.limiter.WaitN(q.ctx, len(batch.Comments)); err != nil {
		return err
	}
	if gap := inlineMinWriteGap - q.now().Sub(q.lastWrite); gap > 0 {
		if err := q.sleep(q.ctx, gap); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(q.ctx, inlineRequestTimeout)
	defer cancel()
	err := client.PostInlineBatch(ctx, job.repo, job.pr, job.headSHA, job.summary, batch)
	q.lastWrite = q.now()
	return err
}

// retryDelay follows GitHub's guidance: wait as long as a rate-limit response
// says, otherwise at least a minute, doubling with each attempt.
func retryDelay(err error, attempt int) time.Duration {
	if delay, ok := ghpkg.InlineRateLimitDelay(err); ok && delay > 0 {
		return min(delay, inlineMaxRetryDelay)
	}
	return min(inlineDefaultRetryDelay<<(attempt-1), inlineMaxRetryDelay)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
