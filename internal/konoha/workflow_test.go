package konoha

import (
	"reflect"
	"strings"
	"testing"
)

func TestBatchRuns(t *testing.T) {
	if got := batchRuns(5, 1); !reflect.DeepEqual(got, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}) {
		t.Fatalf("batchRuns(5,1)=%v", got)
	}
	if got := batchRuns(5, 2); !reflect.DeepEqual(got, [][2]int{{0, 2}, {2, 4}, {4, 5}}) {
		t.Fatalf("batchRuns(5,2)=%v", got)
	}
	if got := batchRuns(0, 3); len(got) != 0 {
		t.Fatalf("batchRuns(0,3)=%v", got)
	}
	if got := batchRuns(3, 0); !reflect.DeepEqual(got, [][2]int{{0, 1}, {1, 2}, {2, 3}}) {
		t.Fatalf("batchRuns(3,0) must default to serialized: %v", got)
	}
}

func TestPrBodyFormatted(t *testing.T) {
	a := IssueAnalysis{
		Summary:            "Add a usage example to the README.",
		Requirements:       []string{"show main function call"},
		AcceptanceCriteria: []string{"README has a Go snippet"},
		Constraints:        []string{"docs-only, no infra changes"},
		Risks:              []string{"minimal"},
	}
	in := IssueInput{Repo: "ducklambda", IssueNum: 12}
	b := prBody(in, a)

	for _, want := range []string{
		"Closes ducklambda issue #12",
		"## Summary",
		"## Requirements",
		"## Acceptance criteria",
		"## Constraints",
		"## Risks",
		"- show main function call",
		"Add a usage example to the README.",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("prBody missing %q\n%s", want, b)
		}
	}
}
