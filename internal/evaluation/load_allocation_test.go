package evaluation

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCasesRejectsBlankInputWithinAllocationBudget(t *testing.T) {
	// A small invalid file must not allocate one Case for every newline.
	input := bytes.Repeat([]byte{'\n'}, 128*1024)
	reader := bytes.NewReader(input)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cases, err := LoadCases(reader)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "line 1: blank lines are forbidden") {
		t.Fatalf("expected first-line blank rejection, got %v", err)
	}
	if cases != nil {
		t.Fatal("invalid input returned partial cases")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	const budget = 8 * 1024 * 1024
	if allocated > budget {
		t.Fatalf("128 KiB blank input allocated %d bytes; budget is %d", allocated, budget)
	}
}
