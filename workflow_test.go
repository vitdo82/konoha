package main

import (
	"reflect"
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
