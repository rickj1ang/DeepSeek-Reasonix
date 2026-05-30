package memory

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Set is everything memory loaded for one session: the hierarchical docs and a
// handle to the auto-memory store (whose index is captured at load time). It is
// assembled once at boot and folded into the system prompt by Compose. CWD and
// UserDir are retained so the controller can resolve quick-add targets without
// re-deriving discovery context.
type Set struct {
	Docs    []Source // REASONIX.md / AGENTS.md, ascending precedence
	Store   Store    // auto-memory store (may be a zero/disabled Store)
	Index   string   // MEMORY.md contents at load time
	CWD     string   // project working dir used for discovery
	UserDir string   // user config root (may be "")
}

// Options configures discovery. CWD defaults to "." and UserDir is the user
// config root (config.MemoryUserDir()); a "" UserDir disables user-global docs
// and the auto-memory store.
type Options struct {
	CWD     string
	UserDir string
}

// Load discovers all memory for a session: the hierarchical docs and the
// auto-memory index. It is best-effort and never errors — missing files just
// mean less memory — so boot can call it unconditionally.
func Load(opts Options) *Set {
	cwd := opts.CWD
	if cwd == "" {
		cwd = "."
	}
	store := StoreFor(opts.UserDir, cwd)
	return &Set{
		Docs:    discoverDocs(cwd, opts.UserDir),
		Store:   store,
		Index:   store.Index(),
		CWD:     cwd,
		UserDir: opts.UserDir,
	}
}

// DocPath returns the doc-memory file a given scope writes to: ScopeUser →
// <userDir>/REASONIX.md, ScopeLocal → <cwd>/REASONIX.local.md, anything else →
// <cwd>/REASONIX.md (the shared, committed project file). Returns "" for
// ScopeUser when no user dir is configured.
func (s *Set) DocPath(scope Scope) string {
	switch scope {
	case ScopeUser:
		if s.UserDir == "" {
			return ""
		}
		return filepath.Join(s.UserDir, "REASONIX.md")
	case ScopeLocal:
		return filepath.Join(s.CWD, "REASONIX.local.md")
	default:
		return filepath.Join(s.CWD, "REASONIX.md")
	}
}

// Empty reports whether the set carries nothing to inject, so Compose can leave
// the base prompt byte-for-byte untouched (and the cache prefix maximal) when
// there is no memory at all.
func (s *Set) Empty() bool {
	return s == nil || (len(s.Docs) == 0 && strings.TrimSpace(s.Index) == "")
}

// docScopes are the scopes the panel can target for a quick-add or a new doc.
// Ordered broad → specific for display.
var docScopes = []Scope{ScopeUser, ScopeProject, ScopeLocal}

// allowedDocPaths is the closed set of files WriteDoc / AppendDoc may touch: the
// canonical file for each writable scope, plus every doc already discovered this
// session (so an ancestor or AGENTS.md the user is already editing stays
// editable). Keyed by absolute path. This bounds frontend-driven writes to real
// memory files rather than arbitrary paths.
func (s *Set) allowedDocPaths() map[string]bool {
	allow := map[string]bool{}
	for _, sc := range docScopes {
		if p := s.DocPath(sc); p != "" {
			allow[absOf(p)] = true
		}
	}
	for _, d := range s.Docs {
		allow[absOf(d.Path)] = true
	}
	return allow
}

// WriteDoc overwrites a doc-memory file with body, after checking path is a
// recognized memory file (see allowedDocPaths). It is the save side of the
// desktop panel's in-place editor. The write lands on disk immediately but does
// NOT mutate the cache-stable system prefix — the edit folds into the prefix on
// the next session; to make it apply this session, the controller separately
// queues a turn-tail note. Returns the path written.
func (s *Set) WriteDoc(path, body string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("memory unavailable")
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("no path given")
	}
	if !s.allowedDocPaths()[absOf(path)] {
		return "", fmt.Errorf("refusing to write %q: not a recognized memory file", path)
	}
	return path, writeDocFile(path, body)
}

// Block renders the memory as a single Markdown section, or "" when empty. It is
// deterministic given the same files, which is what keeps it a stable cache
// prefix across sessions that don't change their memory.
func (s *Set) Block() string {
	if s.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Memory\n\n")
	b.WriteString("Persistent context loaded from memory files. Treat it as durable, user-authored guidance for this project.\n")

	for _, d := range s.Docs {
		fmt.Fprintf(&b, "\n## %s (%s)\n\n%s\n", d.Path, d.Scope, strings.TrimSpace(d.Body))
	}

	if idx := strings.TrimSpace(s.Index); idx != "" {
		b.WriteString("\n## Saved memories\n\n")
		b.WriteString("You have previously saved these memories. Read the linked file with read_file when one looks relevant; save new durable facts with the `remember` tool.\n\n")
		b.WriteString(idx)
		fmt.Fprintf(&b, "\n\n(stored under %s)\n", s.Store.Dir)
	}
	return b.String()
}

// Compose folds the memory block onto the base system prompt and returns the
// durable cached-prefix string. Base stays first (it is the most stable text, so
// it remains a valid cache prefix even when memory changes between sessions);
// memory follows. With no memory, base is returned unchanged.
func Compose(base string, s *Set) string {
	block := s.Block()
	if block == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return block
	}
	return strings.TrimRight(base, "\n") + "\n\n" + block
}
