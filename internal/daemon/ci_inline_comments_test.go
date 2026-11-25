package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

const inlineTestFindings = `{"schema_version":3,"summary":"Stored review.","verdict":"fail","findings":[` +
	`{"severity":"low","problem":"Minor naming.","fix":"Rename it.","file":"a.go","start_line":3,"end_line":3},` +
	`{"severity":"high","problem":"State is lost.","fix":"Persist it.","file":"b.go","start_line":10,"end_line":12},` +
	`{"severity":"high","problem":"Whole file.","fix":"Split it.","file":"c.go"}]}`

// seedInlinePanel seeds a panel whose synthesis review has structured findings
// with file and line data, claims it for posting, and returns it.
func seedInlinePanel(t *testing.T, h *ciPollerHarness) (*storage.CIPanel, []storage.BatchReviewResult) {
	t.Helper()
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a",
		[]jobSpec{{Agent: "test", Status: "done", Output: "### High\nState is lost."}})
	h.completeSynthesisWithReview(t, synth.ID, "State is lost.")
	_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, inlineTestFindings, synth.ID)
	require.NoError(t, err)
	won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
	require.NoError(t, err)
	require.True(t, won)
	members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
	require.NoError(t, err)
	return panel, members
}

func captureInlineReviews(h *ciPollerHarness) *[]inlineReviewJob {
	var got []inlineReviewJob
	h.Poller.enqueueInlineReviewFn = func(job inlineReviewJob) {
		got = append(got, job)
	}
	return &got
}

// With inline_comments on, the posted findings that name lines become one
// line-comment review pinned to the panel's head, after the PR comment.
func TestCIPanelPostsInlineReview(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.InlineComments = true
	h.Cfg.CI.CommentMaxFindings = 2
	comments := h.CaptureComments()
	inline := captureInlineReviews(h)
	panel, members := seedInlinePanel(t, h)

	h.Poller.postPanelComment(panel, members)

	require.Len(t, *comments, 1)
	require.Len(t, *inline, 1)
	got := (*inline)[0]
	assert.Equal(t, "acme/api", got.repo)
	assert.Equal(t, 7, got.pr)
	assert.Equal(t, "head-a", got.headSHA)
	assert.Contains(t, got.summary, "The roborev PR comment has the full review.")
	// The cap keeps the two high findings; the whole-file one names no line.
	assert.Equal(t, []reviewpkg.InlineFinding{
		{Severity: "high", Problem: "State is lost.", Fix: "Persist it.", File: "b.go", StartLine: 10, EndLine: 12},
	}, got.findings)
}

func TestCIPanelSkipsInlineReviewByDefault(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	comments := h.CaptureComments()
	inline := captureInlineReviews(h)
	panel, members := seedInlinePanel(t, h)

	h.Poller.postPanelComment(panel, members)

	assert.Len(t, *comments, 1)
	assert.Empty(t, *inline)
}

// Queueing line comments happens after finalization, so the panel's outcome
// and status are already the PR comment's.
func TestCIPanelInlineReviewQueuedAfterFinalize(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.InlineComments = true
	h.CaptureComments()
	statuses := h.CaptureCommitStatuses()
	var outcomeAtEnqueue *string
	h.Poller.enqueueInlineReviewFn = func(inlineReviewJob) {
		stored, err := h.DB.GetCIPanelByPRSHA("acme/api", 7, "head-a")
		require.NoError(t, err)
		outcomeAtEnqueue = stored.Outcome
	}
	panel, members := seedInlinePanel(t, h)

	h.Poller.postPanelComment(panel, members)

	require.NotNil(t, outcomeAtEnqueue)
	assert.Equal(t, storage.PanelOutcomeReviewPosted, *outcomeAtEnqueue)
	require.NotEmpty(t, *statuses)
	assert.Equal(t, "success", (*statuses)[len(*statuses)-1].State)
}

// A PR comment that fails to post gets no line comments either.
func TestCIPanelNoInlineReviewWhenCommentFails(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.InlineComments = true
	h.Poller.postPRCommentFn = func(string, int, string) error { return errors.New("500 Internal Server Error") }
	h.CaptureCommitStatuses()
	inline := captureInlineReviews(h)
	panel, members := seedInlinePanel(t, h)

	h.Poller.postPanelComment(panel, members)

	assert.Empty(t, *inline)
}
