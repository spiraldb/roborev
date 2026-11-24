package review

import (
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// A cap must select the most severe eligible findings, preserving ties and the source.
func TestCommentMaxFindings(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Problem: "low-one"},
		{Severity: "high", Problem: "high-one"},
		{Severity: "critical", Problem: "critical-one"},
		{Severity: "high", Problem: "high-two"},
	}}
	original := slices.Clone(doc.Findings)
	for _, tc := range []struct {
		cap     int
		want    []string
		omitted string
	}{
		{0, []string{"critical-one", "high-one", "high-two"}, ""},
		{1, []string{"critical-one"}, "2 more findings in the full review: `roborev show --job 123`"},
		{2, []string{"critical-one", "high-one"}, "1 more finding in the full review: `roborev show --job 123`"},
		{9, []string{"critical-one", "high-one", "high-two"}, ""},
	} {
		t.Run(fmt.Sprint(tc.cap), func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: "high", MaxFindings: tc.cap, FullReviewReference: "roborev show --job 123"}, ReviewResult{Structured: &doc})
			var got []string
			for _, f := range prepared.findings {
				got = append(got, f.problem)
			}
			assert.Equal(t, tc.want, got)
			body := FormatComment(prepared)
			if tc.omitted != "" {
				assert.Contains(t, body, tc.omitted)
			} else {
				assert.NotContains(t, body, "more finding")
			}
			assert.Equal(t, original, doc.Findings)
			assert.False(t, prepared.IsClean())
		})
	}
	prose := "### High\nFirst\n\n### High\nSecond"
	assert.Equal(t, prose, FormatComment(PrepareComment(CommentConfig{MaxFindings: 1}, ReviewResult{Output: prose})))
	assert.Contains(t, FormatComment(PrepareComment(CommentConfig{MinSeverity: "high", MaxFindings: 1}, ReviewResult{Output: prose})), "Second")
}

func TestCommentCleanMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result ReviewResult
		clean  bool
	}{
		{"legacy filtered", ReviewResult{StructuredOutput: jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"### Low\nMinor","recorded_verdict":false}}`)}, false},
		{"structured empty", ReviewResult{Structured: &StructuredReview{}}, true},
		{"structured filtered", ReviewResult{Structured: &StructuredReview{Findings: []StructuredFinding{{Severity: "low", Problem: "minor"}}}}, true},
		{"parsed prose filtered", ReviewResult{Output: "### Low\nMinor"}, true},
		{"unlabelled survives", ReviewResult{Output: "Unlabelled concern\n\n### Low\nMinor"}, false},
		{"clean looking prose", ReviewResult{Output: "No issues found."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.clean, PrepareComment(CommentConfig{MinSeverity: "high"}, tc.result).IsClean())
		})
	}
}

func FuzzCommentMaxFindings(f *testing.F) {
	f.Add([]byte{0, 3, 1, 3, 2}, uint8(2), uint8(1))
	f.Add([]byte{}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, capByte, thresholdByte uint8) {
		severities := []string{"low", "medium", "high", "critical"}
		var doc StructuredReview
		for i, b := range data {
			doc.Findings = append(doc.Findings, StructuredFinding{Severity: severities[int(b)%4], Problem: fmt.Sprint(i)})
		}
		cap := int(capByte)
		threshold := severities[int(thresholdByte)%4]
		got := PrepareComment(CommentConfig{MinSeverity: threshold, MaxFindings: cap}, ReviewResult{Structured: &doc})
		// Independently build the expected stable severity buckets from the input.
		var want []string
		for rank := 3; rank >= int(thresholdByte)%4; rank-- {
			for i, b := range data {
				if int(b)%4 == rank {
					want = append(want, fmt.Sprint(i))
				}
			}
		}
		if cap > 0 && len(want) > cap {
			want = want[:cap]
		}
		require.Len(t, got.findings, len(want))
		for i, w := range want {
			assert.Equal(t, w, got.findings[i].problem)
			assert.GreaterOrEqual(t, config.SeverityRank(got.findings[i].severity), config.SeverityRank(threshold))
		}
	})
}

func TestCommentMinConfidence(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "high", Problem: "legacy confidence", Confidence: 0},
		{Severity: "high", Problem: "explicit zero", Kind: "defect", Confidence: 0},
		{Severity: "high", Problem: "below floor", Kind: "defect", Confidence: 0.69},
		{Severity: "high", Problem: "at floor", Kind: "defect", Confidence: 0.7},
		{Severity: "high", Problem: "confident", Kind: "defect", Confidence: 0.9},
		{Severity: "critical", Problem: "critical", Kind: "defect", Confidence: 0.7},
	}}
	original := slices.Clone(doc.Findings)
	for _, tc := range []struct {
		name    string
		floor   float64
		cap     int
		want    []string
		omitted int
	}{
		{"off", 0, 0, []string{"critical", "legacy confidence", "explicit zero", "below floor", "at floor", "confident"}, 0},
		{"inclusive floor", 0.7, 0, []string{"critical", "legacy confidence", "at floor", "confident"}, 0},
		{"cap after filters", 0.7, 3, []string{"critical", "legacy confidence", "at floor"}, 1},
		{"missing survives", 1, 0, []string{"legacy confidence"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: "high", MinConfidence: tc.floor, MaxFindings: tc.cap}, ReviewResult{Structured: &doc})
			var got []string
			for _, finding := range prepared.findings {
				got = append(got, finding.problem)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.omitted, prepared.omitted)
			assert.Equal(t, original, doc.Findings)
		})
	}
	for _, tc := range []struct {
		name        string
		minSeverity string
		findings    []StructuredFinding
		message     string
	}{
		{name: "already clean", message: "No issues found."},
		{name: "severity only", minSeverity: "high", findings: []StructuredFinding{{Severity: "low", Kind: "defect", Confidence: 0}}, message: "No findings at or above high severity."},
		{name: "confidence removed eligible finding", minSeverity: "high", findings: []StructuredFinding{{Severity: "high", Kind: "defect", Confidence: 0}}, message: "No findings match the comment filters."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: tc.minSeverity, MinConfidence: 0.7}, ReviewResult{Structured: &StructuredReview{Findings: tc.findings}})
			assert.True(t, prepared.IsClean())
			assert.Equal(t, "**Verdict:** "+tc.message+"\n", FormatComment(prepared))
		})
	}
}

// Inline findings must be the displayed findings that name lines, in display order.
func TestInlineFindingsFollowDisplayedFindings(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Problem: "low-one", File: "a.go", StartLine: 3, EndLine: 3},
		{Severity: "high", Problem: "high-range", Fix: "fix it", File: "b.go", StartLine: 10, EndLine: 14},
		{Severity: "critical", Problem: "whole-file", File: "c.go"},
		{Severity: "high", Problem: "no-file"},
		{Severity: "medium", Problem: "single-line", File: "d.go", StartLine: 7},
		{Severity: "medium", Problem: "capped", File: "e.go", StartLine: 1, EndLine: 2},
	}}
	prepared := PrepareComment(CommentConfig{MinSeverity: "medium", MaxFindings: 4}, ReviewResult{Structured: &doc})

	assert.Equal(t, []InlineFinding{
		{Severity: "high", Problem: "high-range", Fix: "fix it", File: "b.go", StartLine: 10, EndLine: 14},
		{Severity: "medium", Problem: "single-line", File: "d.go", StartLine: 7, EndLine: 7},
	}, prepared.InlineFindings())
}

// Prose and verbatim reviews carry no line data, so they yield no inline findings.
func TestInlineFindingsEmptyForProse(t *testing.T) {
	prepared := PrepareComment(CommentConfig{MinSeverity: "high"}, ReviewResult{Output: "### High\nState is lost at a.go:3."})
	assert.Empty(t, prepared.InlineFindings())
}

// An end line before the start line is treated as a single-line finding.
func TestInlineFindingsClampInvertedRange(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "high", Problem: "inverted", File: "a.go", StartLine: 9, EndLine: 4},
	}}
	got := PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc}).InlineFindings()
	require.Len(t, got, 1)
	assert.Equal(t, 9, got[0].StartLine)
	assert.Equal(t, 9, got[0].EndLine)
}
