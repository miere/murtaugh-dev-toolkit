// Command archcheck runs Murtaugh's architecture-guard analyzers over the tree.
// It is wired into CI (`go run ./cmd/archcheck ./...`) so a violation fails the
// build. Today it carries the workdir guard (no downstream reads of
// config.AgentProfile.WorkDir); add future arch invariants as analyzers here.
package main

import (
	"github.com/miere/murtaugh/internal/archtest/workdiranalyzer"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(workdiranalyzer.Analyzer)
}
