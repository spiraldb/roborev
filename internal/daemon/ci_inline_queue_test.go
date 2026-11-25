package daemon

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	googlegithub "github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	ghpkg "go.kenn.io/roborev/internal/github"
	reviewpkg "go.kenn.io/roborev/internal/review"
)

// fakeInlineClient plans a fixed set of batches, minus those it has accepted,
// and answers each post from a script of errors.
type fakeInlineClient struct {
	mu       sync.Mutex
	batches  []ghpkg.InlineBatch
	accepted map[int]bool
	posts    []ghpkg.InlineBatch
	planErr  error
	postErrs []error // consumed one per post; nil or exhausted means success
	plans    int
}

func (f *fakeInlineClient) PlanInlineReview(context.Context, string, int, string, []reviewpkg.InlineFinding) ([]ghpkg.InlineBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans++
	if f.planErr != nil {
		return nil, f.planErr
	}
	var pending []ghpkg.InlineBatch
	for _, batch := range f.batches {
		if !f.accepted[batch.Index] {
			pending = append(pending, batch)
		}
	}
	return pending, nil
}

func (f *fakeInlineClient) PostInlineBatch(_ context.Context, _ string, _ int, _, _ string, batch ghpkg.InlineBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, batch)
	var err error
	if len(f.postErrs) > 0 {
		err, f.postErrs = f.postErrs[0], f.postErrs[1:]
	}
	if err == nil {
		if f.accepted == nil {
			f.accepted = map[int]bool{}
		}
		f.accepted[batch.Index] = true
	}
	return err
}

func testBatches(n int) []ghpkg.InlineBatch {
	batches := make([]ghpkg.InlineBatch, n)
	for i := range batches {
		batches[i] = ghpkg.InlineBatch{Index: i, Comments: []ghpkg.InlineComment{{Path: "a.go", StartLine: 1, Line: 2}}}
	}
	return batches
}

// newTestInlineQueue returns a queue whose sleeps are recorded, not taken.
func newTestInlineQueue(client inlineReviewClient) (*inlineReviewQueue, *[]time.Duration) {
	q := newInlineReviewQueue(func(string) (inlineReviewClient, error) { return client, nil })
	q.limiter = rate.NewLimiter(rate.Inf, 1)
	var sleeps []time.Duration
	now := time.Unix(0, 0)
	q.now = func() time.Time { return now }
	q.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		now = now.Add(d)
		return ctx.Err()
	}
	return q, &sleeps
}

func validationError() error {
	return &googlegithub.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}, Message: "Validation Failed"}
}

var inlineJob = inlineReviewJob{repo: "acme/api", pr: 7, headSHA: "head-a", summary: "s"}

// Batches go out one at a time, at least a second apart.
func TestInlineQueuePacesBatches(t *testing.T) {
	client := &fakeInlineClient{batches: testBatches(3)}
	q, sleeps := newTestInlineQueue(client)

	q.process(t.Context(), inlineJob)

	assert.Len(t, client.posts, 3)
	// The first write has no earlier write to wait for.
	assert.Equal(t, []time.Duration{time.Second, time.Second}, *sleeps)
}

// A rate limit waits as long as GitHub says, then re-plans, so batches GitHub
// already accepted are not sent again.
func TestInlineQueueRateLimitWaitsAndResumes(t *testing.T) {
	retry := 42 * time.Second
	client := &fakeInlineClient{batches: testBatches(3), postErrs: []error{nil, &googlegithub.AbuseRateLimitError{RetryAfter: &retry}}}
	q, sleeps := newTestInlineQueue(client)

	q.process(t.Context(), inlineJob)

	var indexes []int
	for _, batch := range client.posts {
		indexes = append(indexes, batch.Index)
	}
	assert.Equal(t, []int{0, 1, 1, 2}, indexes)
	assert.Contains(t, *sleeps, retry)
	assert.Equal(t, 2, client.plans)
}

// Without Retry-After, GitHub asks for at least a minute, doubling per attempt.
func TestInlineQueueGivesUpAfterMaxAttempts(t *testing.T) {
	failure := errors.New("502 Bad Gateway")
	client := &fakeInlineClient{batches: testBatches(1), postErrs: []error{failure, failure, failure, failure}}
	q, sleeps := newTestInlineQueue(client)

	q.process(t.Context(), inlineJob)

	assert.Len(t, client.posts, inlineMaxAttempts)
	var backoffs []time.Duration
	for _, d := range *sleeps {
		if d > time.Second {
			backoffs = append(backoffs, d)
		}
	}
	assert.Equal(t, []time.Duration{time.Minute, 2 * time.Minute}, backoffs)
}

// A batch GitHub cannot place is retried with single-line comments, then
// skipped, and later batches still go out.
func TestInlineQueueValidationFallback(t *testing.T) {
	client := &fakeInlineClient{batches: testBatches(2), postErrs: []error{validationError(), validationError()}}
	q, _ := newTestInlineQueue(client)

	q.process(t.Context(), inlineJob)

	require.Len(t, client.posts, 3)
	assert.Equal(t, 1, client.posts[0].Comments[0].StartLine)
	assert.Equal(t, 0, client.posts[1].Comments[0].StartLine, "the retry uses single-line comments")
	assert.Equal(t, 1, client.posts[2].Index)
	assert.Equal(t, 1, client.plans, "a skipped batch is not a reason to retry the job")
}

func TestInlineQueueHeadMovedStops(t *testing.T) {
	client := &fakeInlineClient{planErr: ghpkg.ErrInlineHeadMoved}
	q, sleeps := newTestInlineQueue(client)

	q.process(t.Context(), inlineJob)

	assert.Equal(t, 1, client.plans)
	assert.Empty(t, client.posts)
	assert.Empty(t, *sleeps)
}

// Shutdown interrupts a worker that is waiting out a backoff.
func TestInlineQueueShutdownDuringBackoff(t *testing.T) {
	failure := errors.New("502 Bad Gateway")
	client := &fakeInlineClient{batches: testBatches(1), postErrs: []error{failure}}
	q := newInlineReviewQueue(func(string) (inlineReviewClient, error) { return client, nil })
	q.limiter = rate.NewLimiter(rate.Inf, 1)
	waiting := make(chan struct{})
	q.sleep = func(ctx context.Context, d time.Duration) error {
		if d >= time.Minute {
			close(waiting)
		}
		return sleepContext(ctx, d)
	}

	q.enqueue(inlineJob)
	<-waiting
	finished := make(chan struct{})
	go func() {
		q.shutdown()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		require.Fail(t, "shutdown did not interrupt the backoff")
	}
	assert.Len(t, client.posts, 1)
}

func TestInlineQueueShutdownWithoutJobs(t *testing.T) {
	q := newInlineReviewQueue(func(string) (inlineReviewClient, error) { return &fakeInlineClient{}, nil })
	q.shutdown()
	q.enqueue(inlineJob) // must not start a worker or block
}
