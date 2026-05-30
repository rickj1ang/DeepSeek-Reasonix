package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"reasonix/internal/tool"
)

// rememberTool lets the model persist a durable fact to the auto-memory store.
// It is stateful (bound to one project's Store), so boot constructs it and adds
// it to the registry — the same pattern as the task tool — rather than
// self-registering as a stateless built-in.
type rememberTool struct{ store Store }

// NewRememberTool returns the `remember` tool bound to store. A zero/disabled
// store yields a tool that reports the store is unavailable rather than silently
// dropping saves.
func NewRememberTool(store Store) tool.Tool { return rememberTool{store: store} }

func (rememberTool) Name() string { return "remember" }

func (rememberTool) Description() string {
	return "Save a durable fact to project memory so it survives across sessions. " +
		"Use for things worth remembering long-term: who the user is and their preferences (type \"user\"); " +
		"guidance on how to work, including the why (type \"feedback\"); ongoing goals or constraints not " +
		"derivable from the code (type \"project\"); or pointers to external resources (type \"reference\"). " +
		"Do NOT save what the repo already records (code structure, git history) or facts that only matter to " +
		"the current conversation. Saving the same name again overwrites it — prefer updating over duplicating. " +
		"The saved index is loaded into context at the start of each session."
}

func (rememberTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Short kebab-case slug identifying the fact, e.g. \"prefers-tabs\". Reusing a name overwrites that memory. Omit to derive one from the description."},
			"description": {"type": "string", "description": "One-line summary used in the memory index and for recall."},
			"type": {"type": "string", "enum": ["user", "feedback", "project", "reference"], "description": "Category of the fact."},
			"body": {"type": "string", "description": "The fact itself (Markdown). For feedback/project, include why it matters and how to apply it."}
		},
		"required": ["description", "body"]
	}`)
}

func (t rememberTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Description == "" || in.Body == "" {
		return "", fmt.Errorf("description and body are required")
	}
	name := in.Name
	if name == "" {
		name = in.Description // Save slugifies; a description makes a serviceable slug
	}
	path, err := t.store.Save(Memory{
		Name:        name,
		Description: in.Description,
		Type:        NormalizeType(in.Type),
		Body:        in.Body,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved memory to %s (it will load automatically in future sessions).", path), nil
}

func (rememberTool) ReadOnly() bool { return false }
