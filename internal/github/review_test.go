package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestDiffLinesIgnoresMalformedHunks(t *testing.T) {
	assert.Empty(t, DiffLines("@@ garbage @@\n+added\n context"))
	assert.Empty(t, DiffLines(""))
}

func TestBuildInlineComments(t *testing.T) {
	files := map[string]map[int]int{"a.go": DiffLines(twoHunkPatch)}
	findings := []review.InlineFinding{
		{Severity: "high", Problem: "range in one hunk", Fix: "do x", File: "a.go", StartLine: 2, EndLine: 4},
		{Severity: "low", Problem: "range across hunks", File: "a.go", StartLine: 3, EndLine: 22},
		{Severity: "medium", Problem: "single line", File: "a.go", StartLine: 23, EndLine: 23},
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
	assert.Equal(t, 0, got[2].StartLine)
	assert.Equal(t, 23, got[2].Line)
	assert.True(t, isRoborevCommentBody(got[0].Body), "roborev must not read its own line comments as discussion")
}

type inlineReviewAPI struct {
	t        *testing.T
	reviews  []*googlegithub.PullRequestReview
	files    []*googlegithub.CommitFile
	created  []googlegithub.PullRequestReviewRequest
	listings int
}

func (api *inlineReviewAPI) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
		api.listings++
		assert.NoError(api.t, json.NewEncoder(w).Encode(api.reviews))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
		assert.NoError(api.t, json.NewEncoder(w).Encode(api.files))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
		var req googlegithub.PullRequestReviewRequest
		assert.NoError(api.t, json.NewDecoder(r.Body).Decode(&req))
		api.created = append(api.created, req)
		api.reviews = append(api.reviews, &googlegithub.PullRequestReview{Body: req.Body})
		assert.NoError(api.t, json.NewEncoder(w).Encode(googlegithub.PullRequestReview{ID: ptr(int64(1))}))
	default:
		api.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPostInlineReview(t *testing.T) {
	api := &inlineReviewAPI{t: t, files: []*googlegithub.CommitFile{
		{Filename: ptr("a.go"), Patch: ptr(twoHunkPatch)},
		{Filename: ptr("image.png")},
	}}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client := newTestGitHubClient(t, "token", server)
	findings := []review.InlineFinding{
		{Severity: "high", Problem: "wrong", Fix: "right", File: "a.go", StartLine: 21, EndLine: 22},
		{Severity: "low", Problem: "not in diff", File: "a.go", StartLine: 10, EndLine: 10},
	}

	posted, err := client.PostInlineReview(t.Context(), "acme/widgets", 7, "abc123", "Summary.", findings)
	require.NoError(t, err)
	assert.Equal(t, 1, posted)
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

	// A retried publication for the same head finds the review and posts nothing.
	posted, err = client.PostInlineReview(t.Context(), "acme/widgets", 7, "abc123", "Summary.", findings)
	require.NoError(t, err)
	assert.Equal(t, 0, posted)
	assert.Len(t, api.created, 1)

	// A new head gets its own review.
	posted, err = client.PostInlineReview(t.Context(), "acme/widgets", 7, "def456", "Summary.", findings)
	require.NoError(t, err)
	assert.Equal(t, 1, posted)
	assert.Len(t, api.created, 2)
}

func TestPostInlineReviewSkipsWhenNothingAnchors(t *testing.T) {
	api := &inlineReviewAPI{t: t, files: []*googlegithub.CommitFile{{Filename: ptr("a.go"), Patch: ptr(twoHunkPatch)}}}
	server := httptest.NewServer(http.HandlerFunc(api.handler))
	defer server.Close()
	client := newTestGitHubClient(t, "token", server)

	posted, err := client.PostInlineReview(t.Context(), "acme/widgets", 7, "abc123", "Summary.",
		[]review.InlineFinding{{Severity: "high", Problem: "elsewhere", File: "b.go", StartLine: 1, EndLine: 1}})
	require.NoError(t, err)
	assert.Equal(t, 0, posted)
	assert.Empty(t, api.created)

	posted, err = client.PostInlineReview(t.Context(), "acme/widgets", 7, "abc123", "Summary.", nil)
	require.NoError(t, err)
	assert.Equal(t, 0, posted)
	assert.Equal(t, 1, api.listings, "no findings means no GitHub calls at all")
}
