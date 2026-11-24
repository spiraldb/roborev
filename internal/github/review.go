package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	googlegithub "github.com/google/go-github/v93/github"

	"go.kenn.io/roborev/internal/review"
)

// InlineMarker is an invisible HTML marker embedded in the pull request
// reviews that roborev posts with line comments, and in each of their
// comments. It lets roborev skip its own line comments when it reads PR
// discussion, and find the batches it already posted for a head commit.
const InlineMarker = "<!-- roborev-inline-review -->"

const (
	// InlineBatchSize is the most line comments roborev puts in one pull
	// request review. GitHub's content-creation limit (80 requests a minute,
	// 500 an hour) does not say how a review's comments count, so roborev
	// assumes each comment counts and keeps reviews small.
	InlineBatchSize = 5
	// InlineMaxCommentsPerHead caps the line comments for one head commit.
	// Findings beyond it stay in the PR comment only.
	InlineMaxCommentsPerHead = 25
)

var inlineBatchMarkerPattern = regexp.MustCompile(`<!-- roborev-inline-head:([0-9a-fA-F]+) batch:(\d+) -->`)

func inlineBatchMarker(headSHA string, batch int) string {
	return fmt.Sprintf("<!-- roborev-inline-head:%s batch:%d -->", headSHA, batch)
}

// ErrInlineHeadMoved reports that the pull request's head is no longer the
// reviewed commit, so line numbers from its diff would not match.
var ErrInlineHeadMoved = errors.New("pull request head moved since the review")

// InlineComment is one line comment of a pull request review. Line is the
// last line on the right (new) side of the diff; StartLine is zero for a
// single-line comment, or the first line of a multi-line comment.
type InlineComment struct {
	Path      string
	StartLine int
	Line      int
	Body      string
}

// InlineBatch is one pull request review's worth of line comments. Index is
// stable for a given head and set of findings, so a retry can tell which
// batches GitHub already accepted.
type InlineBatch struct {
	Index    int
	Comments []InlineComment
}

// SingleLine returns the batch with every multi-line comment shortened to its
// last line, the shape GitHub is least likely to reject.
func (b InlineBatch) SingleLine() InlineBatch {
	out := InlineBatch{Index: b.Index, Comments: make([]InlineComment, len(b.Comments))}
	for i, comment := range b.Comments {
		comment.StartLine = 0
		out.Comments[i] = comment
	}
	return out
}

// DiffLines returns the line numbers on the right (new) side of a unified diff
// patch that GitHub accepts as review comment lines: added lines and context
// lines inside a hunk. Each line maps to the index of its hunk, because a
// multi-line comment must stay within one hunk.
func DiffLines(patch string) map[int]int {
	lines := map[int]int{}
	hunk := -1
	next, remaining := 0, 0
	for raw := range strings.SplitSeq(patch, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, "@@") {
			start, count, ok := hunkNewRange(line)
			if !ok {
				hunk, remaining = -1, 0
				continue
			}
			hunk++
			next, remaining = start, count
			continue
		}
		if hunk < 0 || remaining == 0 {
			continue
		}
		// An empty line inside a hunk is a blank context line whose leading
		// space was stripped.
		if line == "" || line[0] == ' ' || line[0] == '+' {
			lines[next] = hunk
			next++
			remaining--
		}
		// Removed lines and "\ No newline at end of file" do not advance the
		// right side.
	}
	return lines
}

// hunkNewRange parses the new-side start and line count from
// "@@ -a,b +c,d @@". A missing count means one line.
func hunkNewRange(header string) (start, count int, ok bool) {
	_, rest, found := strings.Cut(header, " +")
	if !found {
		return 0, 0, false
	}
	field, _, _ := strings.Cut(rest, " ")
	startText, countText, hasCount := strings.Cut(field, ",")
	start, err := strconv.Atoi(startText)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(countText); err != nil || count < 0 {
			return 0, 0, false
		}
	}
	if count == 0 {
		// A deleted file or a hunk with no new-side lines.
		return 0, 0, false
	}
	return start, count, start > 0
}

// BuildInlineComments anchors findings to the lines of the diff that GitHub
// accepts. files maps a path to the result of DiffLines for its patch. A
// finding whose end line is outside the diff gets no comment. A range whose
// start falls outside the end line's hunk is shortened to the end line. At
// most InlineMaxCommentsPerHead comments are returned, in finding order.
func BuildInlineComments(files map[string]map[int]int, findings []review.InlineFinding) []InlineComment {
	var out []InlineComment
	for _, finding := range findings {
		if len(out) == InlineMaxCommentsPerHead {
			break
		}
		lines, ok := files[strings.TrimPrefix(finding.File, "./")]
		if !ok {
			continue
		}
		hunk, ok := lines[finding.EndLine]
		if !ok {
			continue
		}
		comment := InlineComment{Path: strings.TrimPrefix(finding.File, "./"), Line: finding.EndLine, Body: inlineCommentBody(finding)}
		if finding.StartLine < finding.EndLine && sameHunk(lines, finding.StartLine, finding.EndLine, hunk) {
			comment.StartLine = finding.StartLine
		}
		out = append(out, comment)
	}
	return out
}

// BatchInlineComments splits comments into reviews of at most InlineBatchSize.
func BatchInlineComments(comments []InlineComment) []InlineBatch {
	var batches []InlineBatch
	for start := 0; start < len(comments); start += InlineBatchSize {
		end := min(start+InlineBatchSize, len(comments))
		batches = append(batches, InlineBatch{Index: len(batches), Comments: comments[start:end]})
	}
	return batches
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

// PlanInlineReview returns the batches of line comments still to post for
// headSHA: the findings that land on lines of the PR's diff, split into
// reviews of at most InlineBatchSize, minus batches a roborev review for
// headSHA already carries. It returns ErrInlineHeadMoved when the PR's head is
// no longer headSHA, because the diff GitHub serves belongs to the new head.
func (c *Client) PlanInlineReview(ctx context.Context, ghRepo string, prNumber int, headSHA string, findings []review.InlineFinding) ([]InlineBatch, error) {
	if len(findings) == 0 || strings.TrimSpace(headSHA) == "" {
		return nil, nil
	}
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return nil, err
	}
	pr, _, err := readGitHub(ctx, "get pull request", func() (*googlegithub.PullRequest, *googlegithub.Response, error) {
		return c.api.PullRequests.Get(ctx, owner, repo, prNumber)
	})
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(pr.GetHead().GetSHA(), headSHA) {
		return nil, ErrInlineHeadMoved
	}
	posted, err := c.postedInlineBatches(ctx, owner, repo, prNumber, headSHA)
	if err != nil {
		return nil, err
	}
	files, err := c.prDiffLines(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, err
	}
	var pending []InlineBatch
	for _, batch := range BatchInlineComments(BuildInlineComments(files, findings)) {
		if !posted[batch.Index] {
			pending = append(pending, batch)
		}
	}
	return pending, nil
}

// PostInlineBatch posts one batch as a COMMENT review pinned to headSHA.
func (c *Client) PostInlineBatch(ctx context.Context, ghRepo string, prNumber int, headSHA, summary string, batch InlineBatch) error {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return err
	}
	drafts := make([]*googlegithub.DraftReviewComment, len(batch.Comments))
	for i, comment := range batch.Comments {
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
	body := InlineMarker + "\n" + inlineBatchMarker(headSHA, batch.Index) + "\n" + strings.TrimSpace(summary)
	_, _, err = c.api.PullRequests.CreateReview(ctx, owner, repo, prNumber, &googlegithub.PullRequestReviewRequest{
		CommitID: ptr(headSHA),
		Body:     ptr(review.TruncateComment(body)),
		Event:    ptr("COMMENT"),
		Comments: drafts,
	})
	if err != nil {
		return fmt.Errorf("create PR review: %w", err)
	}
	return nil
}

// InlineRateLimitDelay reports whether err is a GitHub rate limit and how long
// to wait before the next write, following GitHub's guidance: Retry-After,
// else the primary limit's reset time, else zero for the caller's default.
func InlineRateLimitDelay(err error) (time.Duration, bool) {
	if errors.Is(err, errSecondaryCooldown) {
		return remainingSecondaryCooldown(), true
	}
	if abuse, ok := errors.AsType[*googlegithub.AbuseRateLimitError](err); ok {
		if abuse.RetryAfter != nil {
			return *abuse.RetryAfter, true
		}
		return 0, true
	}
	if rate, ok := errors.AsType[*googlegithub.RateLimitError](err); ok {
		return max(time.Until(rate.Rate.Reset.Time), 0), true
	}
	return 0, false
}

// IsInlineValidationError reports a 422 from GitHub: a comment GitHub could
// not place, or GitHub treating the request as spam.
func IsInlineValidationError(err error) bool {
	return isGitHubStatus(err, 422)
}

func remainingSecondaryCooldown() time.Duration {
	secondaryCooldowns.Lock()
	defer secondaryCooldowns.Unlock()
	var longest time.Duration
	for _, until := range secondaryCooldowns.until {
		longest = max(longest, time.Until(until))
	}
	return longest
}

func (c *Client) postedInlineBatches(ctx context.Context, owner, repo string, prNumber int, headSHA string) (map[int]bool, error) {
	posted := map[int]bool{}
	opts := &googlegithub.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := readGitHub(ctx, "list pull request reviews", func() ([]*googlegithub.PullRequestReview, *googlegithub.Response, error) {
			return c.api.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
		})
		if err != nil {
			return nil, err
		}
		for _, item := range reviews {
			for _, match := range inlineBatchMarkerPattern.FindAllStringSubmatch(item.GetBody(), -1) {
				if !strings.EqualFold(match[1], headSHA) {
					continue
				}
				if index, err := strconv.Atoi(match[2]); err == nil {
					posted[index] = true
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return posted, nil
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
