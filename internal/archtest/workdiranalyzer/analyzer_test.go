package workdiranalyzer_test

import (
	"testing"

	"github.com/miere/murtaugh/internal/archtest/workdiranalyzer"
	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer runs the guard over the testdata fixture: the `downstream` package
// reads config.AgentProfile.WorkDir (flagged via `// want` markers) and an
// unrelated WorkDir field (not flagged).
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), workdiranalyzer.Analyzer, "downstream")
}
