// Command casecollisions fails when two tracked paths would name the same
// file on a case-insensitive file system, the default on macOS and Windows.
//
// Linux CI checks out such a pair without complaint, so a collision only
// shows up when someone builds on another platform. Two kinds are reported:
//
//   - paths equal ignoring case, for files and for the directories that
//     contain them (web/src/Foo.ts and web/src/foo.ts, or web/Src/a.ts and
//     web/src/b.ts);
//   - JavaScript and TypeScript modules whose paths are equal ignoring case
//     once the module extension is removed (UserDetailTabs.tsx and
//     userDetailTabs.ts), because an extensionless import resolves to either.
//
// Files imported with their extension, such as stylesheets, only collide when
// the whole path does: App.tsx and app.css are fine.
//
// Usage (the repository root is the cwd):
//
//	go run ./scripts/ci/casecollisions
package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode"
)

// moduleExtensions are the extensions TypeScript and bundlers try for an
// extensionless import. Longer suffixes come first so .d.ts wins over .ts.
var moduleExtensions = []string{".d.ts", ".tsx", ".ts", ".jsx", ".js", ".mjs", ".cjs"}

// group is a set of tracked paths that one case-insensitive name would cover.
type group struct {
	module bool // equal only once the module extension is removed
	paths  []string
}

func main() {
	out, err := exec.Command("git", "ls-files", "-z", "--full-name", "--", ":/").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "casecollisions: git ls-files: %v\n", err)
		os.Exit(1)
	}
	var files []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			files = append(files, path)
		}
	}

	groups := collisions(files)
	if len(groups) == 0 {
		fmt.Printf("casecollisions: %d tracked files, no paths collide ignoring case\n", len(files))
		return
	}
	fmt.Print(report(groups))
	os.Exit(1)
}

// collisions returns every group of paths that collide ignoring case, sorted
// by their first path.
func collisions(files []string) []group {
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)

	// Every file and every directory above it.
	names := map[string]bool{}
	for _, file := range files {
		for name := file; name != "" && !names[name]; name = parent(name) {
			names[name] = true
		}
	}
	byFold := map[string][]string{}
	for name := range names {
		key := fold(name)
		byFold[key] = append(byFold[key], name)
	}
	var groups []group
	for _, paths := range byFold {
		if len(paths) > 1 {
			slices.Sort(paths)
			groups = append(groups, group{paths: paths})
		}
	}

	byModule := map[string][]string{}
	for _, file := range files {
		if base, ok := stripModuleExtension(file); ok {
			key := fold(base)
			byModule[key] = append(byModule[key], file)
		}
	}
	for _, paths := range byModule {
		if moduleCollision(paths) {
			groups = append(groups, group{module: true, paths: paths})
		}
	}

	slices.SortFunc(groups, func(a, b group) int {
		return strings.Compare(a.paths[0], b.paths[0])
	})
	return groups
}

// moduleCollision reports whether two modules differ in case once their
// extensions are removed and are not already the same path ignoring case.
// foo.ts and foo.tsx differ only in extension, which is not a case problem;
// Foo.ts and foo.ts are reported as a path collision instead.
func moduleCollision(paths []string) bool {
	for i, a := range paths {
		baseA, _ := stripModuleExtension(a)
		for _, b := range paths[i+1:] {
			baseB, _ := stripModuleExtension(b)
			if baseA != baseB && fold(a) != fold(b) {
				return true
			}
		}
	}
	return false
}

func stripModuleExtension(path string) (string, bool) {
	for _, ext := range moduleExtensions {
		if base, ok := strings.CutSuffix(path, ext); ok && base != "" && !strings.HasSuffix(base, "/") {
			return base, true
		}
	}
	return "", false
}

func parent(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ""
	}
	return path[:i]
}

// fold maps every rune to the smallest rune in its Unicode case-folding
// orbit, so two strings fold equal exactly when strings.EqualFold says so.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			smallest = min(smallest, f)
		}
		return smallest
	}, s)
}

func report(groups []group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "::error::%d group(s) of tracked paths collide on a case-insensitive file system (macOS, Windows); rename all but one path in each group\n", len(groups))
	for _, g := range groups {
		if g.module {
			b.WriteString("\nsame import path ignoring case (an extensionless import can resolve to any of them):\n")
		} else {
			b.WriteString("\nsame path ignoring case:\n")
		}
		for _, path := range g.paths {
			fmt.Fprintf(&b, "  %s\n", path)
		}
	}
	return b.String()
}
