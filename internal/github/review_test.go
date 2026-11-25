package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	googlegithub "github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/review"
)

const twoHunkPatch = "@@ -1,3 +1,4 @@\n" +
	" package a\n" +
	"+import \"fmt\"\n" +
	" \n" +
	" func A() {}\n" +
	"@@ -20,4 +21,3 @@ func B() {\n" +
	" \tx := 1\n" +
	"-\ty := 2\n" +
	"+\ty := 3\n" +
	" \treturn\n" +
	"\\ No newline at end of file"

// Only right-side lines inside hunks are commentable, each tagged with its hunk.
func TestDiffLines(t *testing.T) {
	assert.Equal(t, map[int]int{
		1: 0, 2: 0, 3: 0, 4: 0,
		21: 1, 22: 1, 23: 1,
	}, DiffLines(twoHunkPatch))
}

func TestDiffLinesHeaderAndLineForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch string
		want  map[int]int
	}{
		{"blank context line without its space", "@@ -1,3 +1,3 @@\n a\n\n+c", map[int]int{1: 0, 2: 0, 3: 0}},
		{"new file", "@@ -0,0 +1,2 @@\n+a\n+b\n", map[int]int{1: 0, 2: 0}},
		{"single-line counts omitted", "@@ -1 +1 @@\n-a\n+b", map[int]int{1: 0}},
		{"deleted file", "@@ -1,2 +0,0 @@\n-a\n-b", map[int]int{}},
		{"CRLF", "@@ -1,2 +1,2 @@\r\n a\r\n+b\r\n", map[int]int{1: 0, 2: 0}},
		{"trailing newline after the hunk", "@@ -1,1 +1,1 @@\n+a\n\n", map[int]int{1: 0}},
		{"malformed header", "@@ garbage @@\n+added\n context", map[int]int{}},
		{"empty", "", map[int]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DiffLines(tc.patch))
		})
	}
}

func TestBuildInlineComments(t *testing.T) {
	files := map[string]map[int]int{"a.go": DiffLines(twoHunkPatch)}
	findings := []review.InlineFinding{
		{Severity: "high", Problem: "range in one hunk", Fix: "do x", File: "a.go", StartLine: 2, EndLine: 4},
		{Severity: "low", Problem: "range across hunks", File: "a.go", StartLine: 3, EndLine: 22},
		{Severity: "medium", Problem: "dot-slash path", File: "./a.go", StartLine: 23, EndLine: 23},
		{Severity: "high", Problem: "outside the diff", File: "a.go", StartLine: 10, EndLine: 10},
		{Severity: "high", Problem: "file not in the diff", File: "b.go", StartLine: 1, EndLine: 1},
	}

	got := BuildInlineComments(files, findings)

	require.Len(t, got, 3)
	assert.Equal(t, InlineComment{
		Path: "a.go", StartLine: 2, Line: 4,
		Body: InlineMarker + "\n**High:** range in one hunk\n\n**Fix:** do x",
	}, got[0])
	assert.Equal(t, 0, got[1].StartLine, "a range that leaves its hunk is shortened to its end line")
	assert.Equal(t, 22, got[1].Line)
	assert.Equal(t, "a.go", got[2].Path)
	assert.True(t, isRoborevCommentBody(got[0].Body), "roborev must not read its own line comments as discussion")
}

func TestBuildInlineCommentsCapsAndBatches(t *testing.T) {
	lines := map[int]int{}
	var findings []review.InlineFinding
	for line := 1; line <= 40; line++ {
		lines[line] = 0
		findings = append(findings, review.InlineFinding{Severity: "low", Problem: fmt.Sprint(line), File: "a.go", StartLine: line, EndLine: line})
	}

	comments := BuildInlineComments(map[string]map[int]int{"a.go": lines}, findings)
	require.Len(t, comments, InlineMaxCommentsPerHead)
	assert.Equal(t, 1, comments[0].Line, "the cap keeps findings in display order")

	batches := BatchInlineComments(comments)
	require.Len(t, batches, InlineMaxCommentsPerHead/InlineBatchSize)
	for i, batch := range batches {
		assert.Equal(t, i, batch.Index)
		assert.Len(t, batch.Comments, InlineBatchSize)
	}
	assert.Empty(t, BatchInlineComments(nil))
}

func TestInlineBatchSingleLine(t *testing.T) {
	batch := InlineBatch{Index: 3, Comments: []InlineComment{{Path: "a.go", StartLine: 2, Line: 4}}}
	single := batch.SingleLine()
	assert.Equal(t, 3, single.Index)
	assert.Equal(t, 0, single.Comments[0].StartLine)
	assert.Equal(t, 4, single.Comments[0].Line)
	assert.Equal(t, 2, batch.Comments[0].StartLine, "SingleLine must not modify the original batch")
}

type inlineReviewAPI struct {
	t       *testing.T
	head    string
	reviews []*googlegithub.PullRequestReview
	files   [][]*googlegithub.CommitFile // one slice per page
	created []googlegithub.PullRequestReviewRequest
	status  int // response status for CreateReview; 0 means 200
}

func (api *inlineReviewAPI) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
		assert.NoError(api.t, json.NewEncoder(w).Encode(googlegithub.PullRequest{Head: &googlegithub.PullRequestBranch{SHA: ptr(api.head)}}))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
		assert.NoError(api.t, json.NewEncoder(w).Encode(api.reviews))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
		page := 1
		if value := r.URL.Query().Get("page"); value != "" {
			_, _ = fmt.Sscan(value, &page)
		}
		if page < len(api.files) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
		}
		assert.NoError(api.t, json.NewEncoder(w).Encode(api.files[page-1]))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
		var req googlegithub.PullRequestReviewRequest
		assert.NoError(api.t, json.NewDecoder(r.Body).Decode(&req))
		api.created = append(api.created, req)
		if api.status != 0 {
			w.WriteHeader(api.status)
			_, _ = fmt.Fprint(w, `{"message":"Validation Failed"}`)
			return
		}
		api.reviews = append(api.reviews, &googlegithub.PullRequestReview{Body: req.Body})
		assert.NoError(api.t, json.NewEncoder(w).Encode(googlegithub.PullRequestReview{ID: ptr(int64(1))}))
	default:
		api.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func newInlineReviewServer(t *testing.T, api *inlineReviewAPI) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(server.Close)
	return newTestGitHubClient(t, "token", server)
}

func TestPlanAndPostInlineReview(t *testing.T) {
	api := &inlineReviewAPI{t: t, head: "abc123", files: [][]*googlegithub.CommitFile{
		{{Filename: ptr("image.png")}},
		{{Filename: ptr("a.go"), Patch: ptr(twoHunkPatch)}},
	}}
	client := newInlineReviewServer(t, api)
	findings := []review.InlineFinding{
		{Severity: "high", Problem: "wrong", Fix: "right", File: "a.go", StartLine: 21, EndLine: 22},
		{Severity: "low", Problem: "not in diff", File: "a.go", StartLine: 10, EndLine: 10},
	}

	batches, err := client.PlanInlineReview(t.Context(), "acme/widgets", 7, "abc123", findings)
	require.NoError(t, err)
	require.Len(t, batches, 1, "the second page of files must be read")
	require.NoError(t, client.PostInlineBatch(t.Context(), "acme/widgets", 7, "abc123", "Summary.", batches[0]))

	require.Len(t, api.created, 1)
	req := api.created[0]
	assert.Equal(t, "abc123", req.GetCommitID())
	assert.Equal(t, "COMMENT", req.GetEvent())
	assert.Contains(t, req.GetBody(), InlineMarker)
	assert.Contains(t, req.GetBody(), "Summary.")
	require.Len(t, req.Comments, 1)
	comment := req.Comments[0]
	assert.Equal(t, "a.go", comment.GetPath())
	assert.Equal(t, "RIGHT", comment.GetSide())
	assert.Equal(t, 22, comment.GetLine())
	assert.Equal(t, 21, comment.GetStartLine())
	assert.Equal(t, "RIGHT", comment.GetStartSide())

	// A retry for the same head finds the posted batch and plans nothing.
	batches, err = client.PlanInlineReview(t.Context(), "acme/widgets", 7, "abc123", findings)
	require.NoError(t, err)
	assert.Empty(t, batches)
}

func TestPlanInlineReviewSkipsPostedBatchesOnly(t *testing.T) {
	var findings []review.InlineFinding
	lines := ""
	for line := 1; line <= 12; line++ {
		lines += "+x\n"
		findings = append(findings, review.InlineFinding{Severity: "low", Problem: "p", File: "a.go", StartLine: line, EndLine: line})
	}
	api := &inlineReviewAPI{
		t: t, head: "abc123",
		files: [][]*googlegithub.CommitFile{{{Filename: ptr("a.go"), Patch: ptr("@@ -0,0 +1,12 @@\n" + lines)}}},
		reviews: []*googlegithub.PullRequestReview{
			{Body: ptr(InlineMarker + "\n" + inlineBatchMarker("abc123", 1))},
			{Body: ptr(InlineMarker + "\n" + inlineBatchMarker("old999", 0))},
		},
	}
	client := newInlineReviewServer(t, api)

	batches, err := client.PlanInlineReview(t.Context(), "acme/widgets", 7, "abc123", findings)
	require.NoError(t, err)
	require.Len(t, batches, 2, "batch 1 for this head is already posted; another head's batch 0 does not count")
	assert.Equal(t, 0, batches[0].Index)
	assert.Equal(t, 2, batches[1].Index)
	assert.Len(t, batches[1].Comments, 2)
}

func TestPlanInlineReviewHeadMoved(t *testing.T) {
	api := &inlineReviewAPI{t: t, head: "newhead"}
	client := newInlineReviewServer(t, api)

	_, err := client.PlanInlineReview(t.Context(), "acme/widgets", 7, "abc123",
		[]review.InlineFinding{{Severity: "high", Problem: "p", File: "a.go", StartLine: 1, EndLine: 1}})
	require.ErrorIs(t, err, ErrInlineHeadMoved)
}

func TestPlanInlineReviewNoFindingsMakesNoRequests(t *testing.T) {
	api := &inlineReviewAPI{t: t}
	client := newInlineReviewServer(t, api)

	batches, err := client.PlanInlineReview(t.Context(), "acme/widgets", 7, "abc123", nil)
	require.NoError(t, err)
	assert.Empty(t, batches)
}

func TestPostInlineBatchValidationError(t *testing.T) {
	api := &inlineReviewAPI{t: t, status: http.StatusUnprocessableEntity}
	client := newInlineReviewServer(t, api)

	err := client.PostInlineBatch(t.Context(), "acme/widgets", 7, "abc123", "Summary.",
		InlineBatch{Comments: []InlineComment{{Path: "a.go", Line: 1, Body: "b"}}})
	require.Error(t, err)
	assert.True(t, IsInlineValidationError(err))
	_, limited := InlineRateLimitDelay(err)
	assert.False(t, limited)
}

func TestInlineRateLimitDelay(t *testing.T) {
	retry := 42 * time.Second
	delay, ok := InlineRateLimitDelay(fmt.Errorf("create PR review: %w", &googlegithub.AbuseRateLimitError{RetryAfter: &retry}))
	assert.True(t, ok)
	assert.Equal(t, retry, delay)

	delay, ok = InlineRateLimitDelay(&googlegithub.AbuseRateLimitError{})
	assert.True(t, ok)
	assert.Zero(t, delay, "without Retry-After the caller applies GitHub's one-minute minimum")

	_, ok = InlineRateLimitDelay(fmt.Errorf("boom"))
	assert.False(t, ok)
}
