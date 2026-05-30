// Package memory implements Reasonix's persistent memory. It mirrors Claude
// Code's two-layer model while honoring Reasonix's cache-first architecture:
//
//   - Hierarchical doc memory: REASONIX.md / AGENTS.md files discovered from the
//     user config dir and up the project tree, with "@path" imports. This is the
//     analog of CLAUDE.md.
//   - Auto-memory store: per-project fact files with frontmatter plus a MEMORY.md
//     index, which the model maintains via the `remember` tool (see store.go).
//
// All of it folds into the durable system-prompt prefix exactly once at boot
// (see Compose), so it rides DeepSeek's automatic prefix cache at zero per-turn
// cost. Mid-session changes never mutate that prefix; they take effect through
// the controller's transient tail-injection and fold into the prefix on the next
// session.
package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// Scope labels where a doc source was discovered, so the assembled block can
// attribute each chunk and callers (e.g. the `#` quick-add picker) can offer
// meaningful targets.
type Scope string

const (
	ScopeUser     Scope = "user"     // ~/.config/reasonix/REASONIX.md
	ScopeAncestor Scope = "ancestor" // a REASONIX.md above the project root
	ScopeProject  Scope = "project"  // ./REASONIX.md (committed, shared)
	ScopeLocal    Scope = "local"    // ./REASONIX.local.md (personal, git-ignored)
)

// docNames are the recognized memory filenames at each level, in load order.
// REASONIX.md is canonical; AGENTS.md is the cross-tool fallback. When both
// exist in one directory, both load (each labeled with its source path), so a
// repo already carrying an AGENTS.md is picked up without renaming.
var docNames = []string{"REASONIX.md", "AGENTS.md"}

// localNames are the personal, git-ignored overrides, highest precedence.
var localNames = []string{"REASONIX.local.md", "AGENTS.local.md"}

// maxImportDepth bounds "@path" import recursion (matches Claude Code's limit).
const maxImportDepth = 5

// Source is one loaded memory file with provenance and @import-expanded body.
type Source struct {
	Path  string
	Scope Scope
	Body  string
}

// discoverDocs walks the memory hierarchy and returns the loaded sources in
// ascending precedence order: user-global first, then ancestors from the
// outermost down, then the project root, then project-local. Later sources are
// more specific, so a model reading top-to-bottom sees the most local guidance
// last. Discovery is best-effort: missing or unreadable files are skipped.
func discoverDocs(cwd, userDir string) []Source {
	var out []Source

	// 1. User-global memory (lowest precedence).
	if userDir != "" {
		out = append(out, loadFrom(userDir, docNames, ScopeUser)...)
	}

	// 2. Ancestor chain, outermost → project root. The project root (cwd) is
	//    tagged ScopeProject; everything above it ScopeAncestor.
	for _, dir := range ancestorsToRoot(cwd) {
		scope := ScopeAncestor
		if sameDir(dir, cwd) {
			scope = ScopeProject
		}
		out = append(out, loadFrom(dir, docNames, scope)...)
	}

	// 3. Project-local overrides (highest precedence).
	out = append(out, loadFrom(cwd, localNames, ScopeLocal)...)

	return out
}

// loadFrom loads each present name in dir, in order, expanding @imports relative
// to dir. A name with no file, or one that fails to read, is silently skipped.
func loadFrom(dir string, names []string, scope Scope) []Source {
	var out []Source
	for _, name := range names {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(b))
		if body == "" {
			continue
		}
		body = resolveImports(body, dir, map[string]bool{absOf(path): true}, 0)
		out = append(out, Source{Path: path, Scope: scope, Body: body})
	}
	return out
}

// ancestorsToRoot returns the directory chain from the project root down to cwd,
// outermost first. The project root is the nearest ancestor containing a .git
// entry (inclusive of cwd); if none is found the chain is just cwd, so discovery
// never wanders above an un-versioned working directory.
func ancestorsToRoot(cwd string) []string {
	abs := absOf(cwd)
	root := gitRoot(abs)
	if root == "" {
		return []string{abs}
	}
	var chain []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		chain = append(chain, dir)
		if sameDir(dir, root) {
			break
		}
		if parent := filepath.Dir(dir); parent == dir {
			break // filesystem root reached without matching git root
		}
	}
	// chain is cwd→root; reverse to root→cwd.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// gitRoot returns the nearest ancestor of dir (inclusive) that contains a .git
// entry, or "" if none exists up to the filesystem root.
func gitRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveImports inlines lines that are exactly "@<path>" by replacing them with
// the referenced file's content. Paths resolve relative to baseDir, with a
// leading ~ expanded to home and absolute paths honored as-is. Recurses up to
// maxImportDepth with cycle detection via seen (absolute paths). An import that
// cannot be read is left as-is so the user can see what failed.
func resolveImports(body, baseDir string, seen map[string]bool, depth int) string {
	if depth >= maxImportDepth {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		target, ok := importTarget(line)
		if !ok {
			continue
		}
		path := resolvePath(target, baseDir)
		abs := absOf(path)
		if seen[abs] {
			lines[i] = line + "  <!-- skipped: import cycle -->"
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue // leave the @line untouched; nothing to inline
		}
		seen[abs] = true
		lines[i] = resolveImports(strings.TrimSpace(string(b)), filepath.Dir(path), seen, depth+1)
	}
	return strings.Join(lines, "\n")
}

// importTarget reports whether a line is an import directive ("@<path>", the only
// token on the line) and returns the path. A bare "@" or an "@word" that is
// clearly prose (no path separator and no dot) is ignored, so ordinary
// "@mentions" in memory text aren't mistaken for imports.
func importTarget(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "@") || len(t) == 1 {
		return "", false
	}
	if strings.ContainsAny(t, " \t") {
		return "", false // more than one token: not an import directive
	}
	p := t[1:]
	if !strings.ContainsAny(p, "/\\") && !strings.Contains(p, ".") {
		return "", false
	}
	return p, true
}

// resolvePath turns an import token into a filesystem path: ~ expands to home,
// absolute paths pass through, everything else is relative to baseDir.
func resolvePath(p, baseDir string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// absOf returns the absolute form of p, falling back to a cleaned p on error so
// the value is still usable as a stable map key.
func absOf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// sameDir reports whether two paths denote the same directory.
func sameDir(a, b string) bool { return absOf(a) == absOf(b) }
