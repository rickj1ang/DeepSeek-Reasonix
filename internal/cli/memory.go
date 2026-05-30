package cli

import "fmt"

// showMemory reports what memory is loaded and where it lives — the TUI analog
// of Claude Code's /memory. It surfaces the doc files and the auto-memory store
// path so the user can open and edit them directly, since the in-terminal UI
// doesn't shell out to an editor.
func (m *chatTUI) showMemory() {
	set := m.ctrl.Memory()
	if set == nil || set.Empty() {
		m.notice("memory: none — add with “#<note>” or create REASONIX.md in the project root")
		return
	}
	m.notice("memory loaded:")
	for _, d := range set.Docs {
		m.notice(fmt.Sprintf("  • %s (%s)", d.Path, d.Scope))
	}
	if set.Index != "" {
		m.notice("  • saved memories → " + set.Store.Dir)
	}
	m.notice("edit those files or use “#<note>”; changes apply next session")
}
