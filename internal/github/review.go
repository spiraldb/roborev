package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	googlegithub "github.com/google/go-github/v93/github"

	"go.kenn.io/roborev/internal/review"
)

// InlineMarker is an invisible HTML marker embedded in the pull request
// review that roborev posts with line comments, and in each of its comments.
// It lets roborev skip its own line comments when it reads PR discussion, and
// find the review it already posted for a head commit.
const InlineMarker = "<!-- roborev-inline-review -->"

// inlineHeadMarker records which head commit a posted review covers, so a
// retried publication does not post a second review for the same commit.
func inlineHeadMarker(headSHA string) string {
	return "<!-- roborev-inline-head:" + headSHA + " -->"
}

// InlineComment is one line comment of a pull request review. Line is the
// last line on the right (new) side of the diff; StartLine is zero for a
// single-line comment, or the first line of a multi-line comment.
type InlineComment struct {
	Path      string
	StartLine int
	Line      int
	Body      string
}

// DiffLines returns the line numbers on the right (new) side of a unified diff
// patch that GitHub accepts as review comment lines: added lines and context
// lines inside a hunk. Each line maps to the index of its hunk, because a
// multi-line comment must stay within one hunk.
func DiffLines(patch string) map[int]int {
	lines := map[int]int{}
	hunk := -1
	next := 0
	for raw := range strings.SplitSeq(patch, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, "@@") {
			start, ok := hunkNewStart(line)
			if !ok {
				hunk = -1
				continue
			}
			hunk++
			next = start
			continue
		}
		if hunk < 0 || line == "" {
			continue
		}
		switch line[0] {
		case '+', ' ':
			lines[next] = hunk
			next++
		case '-', '\\':
			// Removed lines and "\ No newline at end of file" do not
			// advance the right side.
		}
	}
	return lines
}

// hunkNewStart parses the new-side start line from "@@ -a,b +c,d @@".
func hunkNewStart(header string) (int, bool) {
	_, rest, ok := strings.Cut(header, " +")
	if !ok {
		return 0, false
	}
	field, _, _ := strings.Cut(rest, " ")
	field, _, _ = strings.Cut(field, ",")
	start, err := strconv.Atoi(field)
	if err != nil || start <= 0 {
		return 0, false
	}
	return start, true
}

// BuildInlineComments anchors findings to the lines of the diff that GitHub
// accepts. files maps a path to the result of DiffLines for its patch. A
// finding whose end line is outside the diff gets no comment. A range whose
// start falls outside the end line's hunk is shortened to the end line.
func BuildInlineComments(files map[string]map[int]int, findings []review.InlineFinding) []InlineComment {
	var out []InlineComment
	for _, finding := range findings {
		lines, ok := files[finding.File]
		if !ok {
			continue
		}
		hunk, ok := lines[finding.EndLine]
		if !ok {
			continue
		}
		comment := InlineComment{Path: finding.File, Line: finding.EndLine, Body: inlineCommentBody(finding)}
		if finding.StartLine < finding.EndLine && sameHunk(lines, finding.StartLine, finding.EndLine, hunk) {
			comment.StartLine = finding.StartLine
		}
		out = append(out, comment)
	}
	return out
}

func sameHunk(lines map[int]int, start, end, hunk int) bool {
	for line := start; line <= end; line++ {
		if h, ok := lines[line]; !ok || h != hunk {
			return false
		}
	}
	return true
}

func inlineCommentBody(finding review.InlineFinding) string {
	var body strings.Builder
	body.WriteString(InlineMarker + "\n")
	if severity := strings.TrimSpace(finding.Severity); severity != "" {
		fmt.Fprintf(&body, "**%s:** ", strings.ToUpper(severity[:1])+strings.ToLower(severity[1:]))
	}
	body.WriteString(strings.TrimSpace(finding.Problem))
	if fix := strings.TrimSpace(finding.Fix); fix != "" {
		fmt.Fprintf(&body, "\n\n**Fix:** %s", fix)
	}
	return review.TruncateComment(body.String())
}

// PostInlineReview posts one pull request review that comments on the lines
// the findings name, pinned to headSHA. It returns the number of line
// comments posted. It posts nothing, and returns zero, when no finding falls
// on a line in the diff or when a roborev review for headSHA already exists.
func (c *Client) PostInlineReview(ctx context.Context, ghRepo string, prNumber int, headSHA, summary string, findings []review.InlineFinding) (int, error) {
	if len(findings) == 0 || strings.TrimSpace(headSHA) == "" {
		return 0, nil
	}
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return 0, err
	}
	posted, err := c.hasInlineReview(ctx, owner, repo, prNumber, headSHA)
	if err != nil || posted {
		return 0, err
	}
	files, err := c.prDiffLines(ctx, owner, repo, prNumber)
	if err != nil {
		return 0, err
	}
	comments := BuildInlineComments(files, findings)
	if len(comments) == 0 {
		return 0, nil
	}
	drafts := make([]*googlegithub.DraftReviewComment, len(comments))
	for i, comment := range comments {
		draft := &googlegithub.DraftReviewComment{
			Path: ptr(comment.Path),
			Body: ptr(comment.Body),
			Side: ptr("RIGHT"),
			Line: ptr(comment.Line),
		}
		if comment.StartLine > 0 {
			draft.StartLine = ptr(comment.StartLine)
			draft.StartSide = ptr("RIGHT")
		}
		drafts[i] = draft
	}
	body := InlineMarker + "\n" + inlineHeadMarker(headSHA) + "\n" + strings.TrimSpace(summary)
	_, _, err = c.api.PullRequests.CreateReview(ctx, owner, repo, prNumber, &googlegithub.PullRequestReviewRequest{
		CommitID: ptr(headSHA),
		Body:     ptr(review.TruncateComment(body)),
		Event:    ptr("COMMENT"),
		Comments: drafts,
	})
	if err != nil {
		return 0, fmt.Errorf("create PR review: %w", err)
	}
	return len(comments), nil
}

func (c *Client) hasInlineReview(ctx context.Context, owner, repo string, prNumber int, headSHA string) (bool, error) {
	marker := inlineHeadMarker(headSHA)
	opts := &googlegithub.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := readGitHub(ctx, "list pull request reviews", func() ([]*googlegithub.PullRequestReview, *googlegithub.Response, error) {
			return c.api.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
		})
		if err != nil {
			return false, err
		}
		for _, item := range reviews {
			if strings.Contains(item.GetBody(), marker) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) prDiffLines(ctx context.Context, owner, repo string, prNumber int) (map[string]map[int]int, error) {
	files := map[string]map[int]int{}
	opts := &googlegithub.ListOptions{PerPage: 100}
	for {
		page, resp, err := readGitHub(ctx, "list pull request files", func() ([]*googlegithub.CommitFile, *googlegithub.Response, error) {
			return c.api.PullRequests.ListFiles(ctx, owner, repo, prNumber, opts)
		})
		if err != nil {
			return nil, err
		}
		for _, file := range page {
			// GitHub omits the patch for binary and very large files, which
			// therefore cannot carry line comments.
			if patch := file.GetPatch(); patch != "" {
				files[file.GetFilename()] = DiffLines(patch)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return files, nil
		}
		opts.Page = resp.NextPage
	}
}
