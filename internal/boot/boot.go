// Package boot assembles a ready-to-drive control.Controller from configuration:
// it loads config, resolves the model(s), builds the tool registry (built-ins +
// plugins), wires the permission gate, and constructs the executor — optionally
// wrapping it in a two-model Coordinator. It is the one place that turns "what the
// user configured" into "a Controller a frontend can drive", so every frontend —
// the terminal TUI, the HTTP/SSE server, the desktop webview — shares the exact
// same assembly instead of each re-deriving it. Frontends pass only a sink and a
// couple of run knobs; everything else comes from config.
package boot

import (
	"context"
	"fmt"
	"os"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/memory"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
)

// Options carries the per-run knobs a frontend chooses; everything else is read
// from configuration. Model "" falls back to the configured default_model;
// MaxSteps 0 uses the config/default. RequireKey forces the executor's API key to
// be present (run/serve pass true so a missing key fails fast; chat/desktop pass
// false so the UI is reachable before a key is set). Sink receives the agent's
// typed event stream.
type Options struct {
	Model      string
	MaxSteps   int
	RequireKey bool
	Sink       event.Sink
}

// Build loads config, resolves the model(s), and returns a Controller wrapping a
// single Agent, or a two-model Coordinator when agent.planner_model is set. The
// returned controller owns plugin subprocesses; call Close (via Controller.Close)
// to release them.
func Build(ctx context.Context, opts Options) (*control.Controller, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	modelName := opts.Model
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	entry, ok := cfg.Provider(modelName)
	if !ok {
		return nil, fmt.Errorf("unknown model %q (configured: %s)", modelName, providerNames(cfg))
	}
	if opts.RequireKey {
		if err := cfg.Validate(modelName); err != nil {
			return nil, err
		}
	}

	execProv, err := NewProvider(entry)
	if err != nil {
		return nil, err
	}

	sysPrompt, err := cfg.ResolveSystemPrompt()
	if err != nil {
		return nil, err
	}

	// Persistent memory (REASONIX.md / AGENTS.md hierarchy + auto-memory index)
	// folds into the system prompt exactly here, once: it becomes part of the
	// durable, cache-stable prefix every turn reuses, so memory costs nothing per
	// turn. Mid-session changes never touch this prefix — they ride the
	// controller's transient turn-injection and fold in on the next session.
	mem := memory.Load(memory.Options{CWD: ".", UserDir: config.MemoryUserDir()})
	sysPrompt = memory.Compose(sysPrompt, mem)

	reg := tool.NewRegistry()
	bashSpec := sandbox.Spec{Mode: cfg.BashMode(), WriteRoots: cfg.WriteRoots(), Network: cfg.Sandbox.Network}
	if bashSpec.Mode == "enforce" && !sandbox.Available() {
		fmt.Fprintln(os.Stderr, "warning: bash sandbox requested but unavailable on this platform; running bash unconfined")
	}
	addBuiltins(reg, cfg.Tools.Enabled, cfg.WriteRoots(), bashSpec)
	cleanup := func() {}
	var pluginHost *plugin.Host
	if len(cfg.Plugins) > 0 {
		host, ptools, err := plugin.StartAll(ctx, PluginSpecs(cfg.Plugins))
		if err != nil {
			return nil, fmt.Errorf("plugin: %w", err)
		}
		pluginHost = host
		cleanup = host.Close
		for _, t := range ptools {
			reg.Add(t)
		}
	}

	maxSteps := cfg.Agent.MaxSteps
	if opts.MaxSteps > 0 {
		maxSteps = opts.MaxSteps
	}

	// Permission policy gates every tool call. The headless gate (no Approver)
	// resolves "ask" to allow — preserving `reasonix run` autonomy — while deny
	// rules hard-block in every mode. Interactive frontends (chat, desktop) swap
	// in an interactive gate later via Controller.EnableInteractiveApproval.
	// Sub-agents always run headless: they have no UI to answer a prompt, so they
	// inherit this same gate.
	policy := permission.New(cfg.Permissions.Mode, cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny)
	headlessGate := permission.NewGate(policy, nil)

	// The `task` tool spawns sub-agents that reuse the parent's provider and
	// tool registry. Wired here after the built-ins / plugins are loaded so
	// sub-agents inherit the full tool set (minus `task` itself, to keep
	// nesting out of the picture). It registers into the same reg the
	// executor uses, so the model surfaces it like any other tool.
	reg.Add(agent.NewTaskTool(execProv, entry.Price, reg, maxSteps,
		entry.ContextWindow, cfg.Agent.Temperature, config.ArchiveDir(), "", headlessGate))

	// The `remember` tool lets the model persist durable facts to the project's
	// auto-memory store; the saved index loads into the prefix on the next session.
	reg.Add(memory.NewRememberTool(mem.Store))

	execSess := agent.NewSession(sysPrompt)
	executor := agent.New(execProv, reg, execSess, agent.Options{
		MaxSteps:      maxSteps,
		Temperature:   cfg.Agent.Temperature,
		Pricing:       entry.Price,
		Gate:          headlessGate,
		ContextWindow: entry.ContextWindow,
		ArchiveDir:    config.ArchiveDir(),
	}, opts.Sink)

	// Custom slash commands (.reasonix/commands + user dir). Best-effort: a malformed
	// file is skipped, and a load error never blocks the session.
	cmds, _ := command.Load(config.CommandDirs()...)

	var runner agent.Runner = executor
	label := modelName

	// Two-model collaboration: a distinct planner_model wraps the executor in a
	// Coordinator with its own session, kept separate for cache stability.
	if pm := cfg.Agent.PlannerModel; pm != "" && pm != modelName {
		pe, ok := cfg.Provider(pm)
		if !ok {
			return nil, fmt.Errorf("planner_model %q is not a configured provider", pm)
		}
		plannerProv, err := NewProvider(pe)
		if err != nil {
			return nil, fmt.Errorf("planner %q: %w", pm, err)
		}
		plannerSess := agent.NewSession(agent.DefaultPlannerPrompt)
		runner = agent.NewCoordinator(plannerProv, plannerSess, pe.Price, executor, cfg.Agent.Temperature, opts.Sink)
		label = modelName + " + planner " + pm
	}

	return control.New(control.Options{
		Runner:       runner,
		Executor:     executor,
		Sink:         opts.Sink,
		Policy:       policy,
		Label:        label,
		SystemPrompt: sysPrompt,
		SessionDir:   config.SessionDir(),
		Host:         pluginHost,
		Commands:     cmds,
		Memory:       mem,
		Cleanup:      cleanup,
	}), nil
}

// NewProvider builds a provider.Provider from a configured entry. Exported so
// custom assemblers (e.g. the ACP per-session factory) can reuse it without
// going through the full Build.
func NewProvider(e *config.ProviderEntry) (provider.Provider, error) {
	return provider.New(e.Kind, provider.Config{
		Name:    e.Name,
		BaseURL: e.BaseURL,
		Model:   e.Model,
		APIKey:  e.APIKey(),
		// Pass the key's env var so auth failures can name where to fix it.
		Extra: map[string]any{"api_key_env": e.APIKeyEnv},
	})
}

// addBuiltins adds enabled built-in tools to reg. An empty list means all of
// them. writeRoots confines the file-writing built-ins to the workspace: after
// the (unconfined) defaults are added, each enabled writer is replaced by an
// instance bound to writeRoots (preserving registry order).
func addBuiltins(reg *tool.Registry, enabled, writeRoots []string, bashSpec sandbox.Spec) {
	if len(enabled) == 0 {
		for _, t := range tool.Builtins() {
			reg.Add(t)
		}
	} else {
		for _, name := range enabled {
			if t, ok := tool.LookupBuiltin(name); ok {
				reg.Add(t)
			} else {
				fmt.Fprintf(os.Stderr, "warning: unknown built-in tool %q\n", name)
			}
		}
	}
	// Replace the unconfined defaults with confined instances (registry order is
	// preserved on replace): file-writers bound to the workspace, bash to the OS
	// sandbox. Only replace tools actually enabled/present.
	confined := append(builtin.ConfineWriters(writeRoots), builtin.ConfineBash(bashSpec))
	for _, t := range confined {
		if _, ok := reg.Get(t.Name()); ok {
			reg.Add(t)
		}
	}
}

// PluginSpecs maps configured plugin entries to plugin.Spec, expanding ${VAR}
// references. Exported so custom assemblers can connect the config's plugins
// alongside their own (e.g. ACP's per-session MCP servers).
func PluginSpecs(entries []config.PluginEntry) []plugin.Spec {
	specs := make([]plugin.Spec, len(entries))
	for i, e := range entries {
		e = e.ExpandedPlugin() // resolve ${VAR} / ${VAR:-default} from the environment
		specs[i] = plugin.Spec{
			Name:    e.Name,
			Type:    e.Type,
			Command: e.Command,
			Args:    e.Args,
			Env:     e.Env,
			URL:     e.URL,
			Headers: e.Headers,
		}
	}
	return specs
}

func providerNames(cfg *config.Config) string {
	names := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, "/")
}
