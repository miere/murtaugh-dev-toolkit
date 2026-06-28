// Package workdiranalyzer is a go/analysis pass enforcing the validated-core rule
// for the agent workdir: downstream packages must not read
// config.AgentProfile.WorkDir directly. The workdir is resolved and validated
// exactly once, at the agentbuild seam (agentbuild.Resolve), and flows downstream
// as a ResolvedAgent / *files.Root. Reading the raw field elsewhere re-opens the
// reactive-defaulting hole this refactor closed (see specs-wip/017).
//
// Detection is type-precise (not textual): it flags a selector whose resolved
// object is the WorkDir field of config.AgentProfile, so it is immune to import
// aliasing, intermediate variables, and struct embedding.
package workdiranalyzer

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer reports downstream reads of config.AgentProfile.WorkDir.
var Analyzer = &analysis.Analyzer{
	Name:     "workdirguard",
	Doc:      "flags downstream access to config.AgentProfile.WorkDir; resolve the workdir via agentbuild.Resolve and consume ResolvedAgent/*files.Root",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// sanctionedSuffixes are the package-path suffixes allowed to read the raw field:
// agentbuild (the seam that owns the fallback) and config (defines/unmarshals it).
var sanctionedSuffixes = []string{
	"/internal/agentbuild",
	"/internal/config",
}

func run(pass *analysis.Pass) (any, error) {
	if isSanctioned(pass.Pkg.Path()) {
		return nil, nil
	}
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, nil
	}
	insp.Preorder([]ast.Node{(*ast.SelectorExpr)(nil)}, func(n ast.Node) {
		sel := n.(*ast.SelectorExpr)
		if sel.Sel == nil || sel.Sel.Name != "WorkDir" {
			return
		}
		selection := pass.TypesInfo.Selections[sel]
		if selection == nil || selection.Kind() != types.FieldVal {
			return
		}
		if !isAgentProfile(selection.Recv()) {
			return
		}
		pass.Reportf(sel.Pos(), "downstream access to config.AgentProfile.WorkDir is forbidden; resolve the workdir once via agentbuild.Resolve and consume ResolvedAgent/*files.Root (see specs-wip/017)")
	})
	return nil, nil
}

func isSanctioned(pkgPath string) bool {
	for _, suffix := range sanctionedSuffixes {
		if pkgPath == strings.TrimPrefix(suffix, "/") || strings.HasSuffix(pkgPath, suffix) {
			return true
		}
	}
	return false
}

// isAgentProfile reports whether t is config.AgentProfile (or a pointer to it).
// Matching is by the type's object name plus its package name ("config"), so a
// self-contained test fixture package named config works without importing the
// real module.
func isAgentProfile(t types.Type) bool {
	named := namedOf(t)
	if named == nil {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Name() != "AgentProfile" {
		return false
	}
	return obj.Pkg() != nil && obj.Pkg().Name() == "config"
}

func namedOf(t types.Type) *types.Named {
	for {
		switch tt := t.(type) {
		case *types.Pointer:
			t = tt.Elem()
		case *types.Named:
			return tt
		default:
			return nil
		}
	}
}
