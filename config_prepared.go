package sam

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
)

// preparedConfig owns the installed configuration and its prompt resources.
// Published maps and slices are immutable. Catalog-only publication replaces
// cfg.Models under mu (catalog readers use mu or configMu), never this owner or
// the unrelated cfg fields read by active workers. New workspaces extend
// a detached copy; only explicit reload refreshes already prepared file paths.
type preparedConfig struct {
	cfg      Config
	tools    map[string]Tool
	toolList []Tool
	skipped  map[string]bool
	files    map[string]string
}

func (p *preparedConfig) text(workspace, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	if text, ok := p.files[path]; ok {
		return text, nil
	}
	text, err := readTextFile(workspace, path)
	if err == nil {
		p.files[path] = text
	}
	return text, err
}

func (p *preparedConfig) preparePrompts(s *sessionRow) error {
	s.restrictDelegation()
	var names []string
	for _, t := range p.enabledTools(s.cfg) {
		names = append(names, t.Name)
	}
	s.resources = PreparedResources{Tools: names, Instructions: []InstructionSource{{Kind: "built-in", Path: "prompts/system.md"}}, Skills: []PreparedSkill{}}
	tools := strings.Join(names, ", ")
	if tools == "" {
		tools = "none"
	}
	documentation := "sam-g reference bundle: not provided by this host."
	if p.cfg.DocumentationDir != "" {
		documentation = strings.TrimSpace(strings.ReplaceAll(prompt("documentation.md"), "{{directory}}", p.cfg.DocumentationDir))
	}
	parts := []string{strings.NewReplacer("{{workspace}}", s.workspace, "{{tools}}", tools, "{{documentation}}", documentation).Replace(prompt("system.md"))}
	if s.cfg.AppendInstructions != "" {
		s.resources.Instructions = append(s.resources.Instructions, InstructionSource{Kind: "inline"})
		parts = append(parts, s.cfg.AppendInstructions)
	}
	if s.cfg.AppendInstructionsFile != "" {
		text, err := p.text(s.workspace, s.cfg.AppendInstructionsFile)
		if err != nil {
			return err
		}
		s.resources.Instructions = append(s.resources.Instructions, InstructionSource{Kind: "file", Path: resourcePath(s.workspace, s.cfg.AppendInstructionsFile)})
		parts = append(parts, text)
	}
	workspaceText, err := p.workspaceInstructions(s)
	if err != nil {
		return err
	}
	if workspaceText != "" {
		parts = append(parts, workspaceText)
	}
	if err := p.prepareSkills(s); err != nil {
		return err
	}
	s.instructions = strings.Join(parts, "\n\n")
	s.compactionPrompt = prompt("compaction.md")
	switch c := s.cfg.Compaction; {
	case c.Prompt != "":
		s.compactionPrompt = c.Prompt
	case c.PromptFile != "":
		text, err := p.text(s.workspace, c.PromptFile)
		if err != nil {
			return err
		}
		s.compactionPrompt = text
	}
	return nil
}

// This is an immutable native-delegation policy, not an OS sandbox. Keep it in
// effective settings/overrides too, so requests and inspection advertise no tool.
func (s *sessionRow) restrictDelegation() {
	if !s.isSubagent {
		return
	}
	if !slices.Contains(s.cfg.DisabledTools, "subagents") {
		s.cfg.DisabledTools = append(slices.Clone(s.cfg.DisabledTools), "subagents")
	}
	s.overrides.DisabledTools = new(slices.Clone(s.cfg.DisabledTools))
}

// Relative default paths have no meaning until a workspace is introduced.
func (p *preparedConfig) prepareAbsolutePrompts() error {
	for _, path := range []string{p.cfg.Defaults.AppendInstructionsFile, p.cfg.Defaults.Compaction.PromptFile} {
		if filepath.IsAbs(path) {
			if _, err := p.text("", path); err != nil {
				return err
			}
		}
	}
	return nil
}

func allSessions(q querier) ([]*sessionRow, error) {
	var sessions []*sessionRow
	err := queryRows(q, func(rows *sql.Rows) error {
		s, err := scanSession(rows)
		if err == nil {
			sessions = append(sessions, s)
		}
		return err
	}, "SELECT "+sessionCols+" FROM sessions ORDER BY rowid")
	return sessions, err
}

func savePrepared(q querier, s *sessionRow) error {
	_, err := q.ExecContext(bg, `UPDATE sessions SET config = ?, overrides = ?, instructions = ?, compaction_prompt = ?, resources = ?, updated_at = ? WHERE id = ?`,
		mustJSON(s.cfg), mustJSON(s.overrides), s.instructions, s.compactionPrompt, mustJSON(s.resources), s.updatedAt, s.id)
	return err
}

// A used chat owns its model/account selection, not the host's next defaults.
// Pin legacy inherited choices from the last saved config before applying new
// defaults. Omitting these overrides later cannot silently change that selection.
func (s *sessionRow) pinSelection() {
	if s.binding == nil {
		return
	}
	if s.overrides.Model == nil {
		s.overrides.Model = new(s.binding.Model)
	}
	if s.overrides.Account == nil {
		s.overrides.Account = new(s.cfg.Account)
	}
}

// Reopen refreshes configuration resources before the Runtime is exposed.
// Existing binding/account incompatibilities remain inspectable and fail at
// execution as before; reopening never rewrites the saved model binding.
func (r *Runtime) prepareOpen(ctx context.Context) error {
	if err := r.prepareAbsolutePrompts(); err != nil {
		return err
	}
	sessions, err := allSessions(r.reader(ctx))
	if err != nil {
		return readErr(ctx, err)
	}
	for _, s := range sessions {
		s.pinSelection()
		s.cfg = s.overrides.apply(r.cfg.Defaults)
		if err := r.preparePrompts(s); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.tx(func(tx *sql.Tx) error {
		for _, s := range sessions {
			if err := savePrepared(tx, s); err != nil {
				return err
			}
		}
		return nil
	})
}
