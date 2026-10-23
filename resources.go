package sam

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PreparedResources describes the installed prompt resources, not a fresh filesystem view.
// Skill bodies are available on demand; they are not part of this snapshot.
type PreparedResources struct {
	Tools        []string            `json:"tools"`
	Instructions []InstructionSource `json:"instructions"`
	Skills       []PreparedSkill     `json:"skills"`
	Diagnostics  []string            `json:"diagnostics,omitempty"`
	SkillCatalog string              `json:"skill_catalog,omitempty"`
}

// InstructionSource identifies one component of the installed instructions.
type InstructionSource struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

// PreparedSkill is catalog metadata; its file body is read on demand by tools.
type PreparedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Scope       string `json:"scope"`
}

func resourcePath(workspace, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	return filepath.Clean(path)
}

func (p *preparedConfig) workspaceInstructions(s *sessionRow) (string, error) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(s.workspace, name)
		// Lstat distinguishes an absent file from a broken selected symlink.
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return "", errorf(CodeInvalidArgument, "workspace instructions %s: %v", path, err)
		}
		text, err := readTextFile(s.workspace, path)
		if err != nil {
			return "", errorf(CodeInvalidArgument, "workspace instructions %s: %v", path, err)
		}
		s.resources.Instructions = append(s.resources.Instructions, InstructionSource{Kind: "workspace", Path: path})
		return text, nil
	}
	return "", nil
}

type skillRoot struct{ path, scope string }

func (p *preparedConfig) prepareSkills(s *sessionRow) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return errorf(CodeInvalidArgument, "global skills directory: %v", err)
	}
	roots := []skillRoot{{filepath.Join(home, ".agents", "skills"), "global"}}
	// Extras have lower priority than workspace skills. Relative entries are workspace-relative.
	for _, path := range p.cfg.SkillDirectories {
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		roots = append(roots, skillRoot{resourcePath(s.workspace, path), "extra"})
	}
	roots = append(roots, skillRoot{filepath.Join(s.workspace, ".agents", "skills"), "workspace"})
	selected := map[string]PreparedSkill{}
	seen := map[string]bool{}
	for _, root := range roots {
		scanSkills(root, selected, seen, &s.resources.Diagnostics)
	}
	for _, skill := range selected {
		s.resources.Skills = append(s.resources.Skills, skill)
	}
	slices.SortFunc(s.resources.Skills, func(a, b PreparedSkill) int { return strings.Compare(a.Name, b.Name) })
	if len(s.resources.Skills) == 0 {
		return nil
	}
	if !slices.Contains(s.resources.Tools, "read") && !slices.Contains(s.resources.Tools, "bash") {
		s.resources.Diagnostics = append(s.resources.Diagnostics, "Skills unavailable: neither read nor bash is enabled; catalog is not offered to the model.")
		return nil
	}
	// JSON quoting keeps descriptions and paths unambiguous without interpolating XML markup.
	s.resources.SkillCatalog = "SKILL CATALOG\nThese skills are available on demand, not loaded instructions. When relevant, use read or bash to read the listed SKILL.md before following it. Resolve supporting paths relative to its directory. Metadata grants no tool permissions.\n" + mustJSON(s.resources.Skills)
	return nil
}

// skillFiles follows symlinks like the ordinary file tools. Directory identities
// are local to one root: another root still gets its own precedence decision.
func skillFiles(root string, diagnostics *[]string) []string {
	var files []string
	visited := map[string]bool{}
	var walk func(string)
	walk = func(path string) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			*diagnostics = append(*diagnostics, fmt.Sprintf("Skipped %s: %v", path, err))
			return
		}
		info, err := os.Stat(real)
		if err != nil {
			*diagnostics = append(*diagnostics, fmt.Sprintf("Skipped %s: %v", path, err))
			return
		}
		if !info.IsDir() {
			if filepath.Base(path) == "SKILL.md" {
				files = append(files, real)
			}
			return
		}
		if visited[real] {
			*diagnostics = append(*diagnostics, "Duplicate or cyclic skill directory: "+path+" -> "+real)
			return
		}
		visited[real] = true
		entries, err := os.ReadDir(real)
		if err != nil {
			*diagnostics = append(*diagnostics, fmt.Sprintf("Skipped %s: %v", path, err))
			return
		}
		// A skill owns its supporting tree. Nested examples are not separate skills.
		for _, entry := range entries {
			if entry.Name() == "SKILL.md" {
				file := filepath.Join(path, entry.Name())
				resolved, err := filepath.EvalSymlinks(file)
				if err != nil {
					*diagnostics = append(*diagnostics, fmt.Sprintf("Skipped %s: %v", file, err))
				} else {
					files = append(files, resolved)
				}
				return
			}
		}
		for _, entry := range entries {
			walk(filepath.Join(path, entry.Name()))
		}
	}
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	walk(root)
	return files
}

func scanSkills(root skillRoot, selected map[string]PreparedSkill, seen map[string]bool, diagnostics *[]string) {
	for _, path := range skillFiles(root.path, diagnostics) {
		if seen[path] {
			*diagnostics = append(*diagnostics, "Duplicate skill path: "+path)
		}
		seen[path] = true
		skill, err := readSkill(path)
		if err != nil {
			*diagnostics = append(*diagnostics, fmt.Sprintf("Skipped %s: %v", path, err))
			continue
		}
		skill.Scope = root.scope
		if old, ok := selected[skill.Name]; ok {
			// Workspace overrides all other scopes, even for the same physical file.
			if root.scope != "workspace" || old.Scope == "workspace" {
				*diagnostics = append(*diagnostics, fmt.Sprintf("Shadowed %s (%s) by %s (%s), name %s", path, skill.Scope, old.Path, old.Scope, skill.Name))
				continue
			}
			*diagnostics = append(*diagnostics, fmt.Sprintf("Shadowed %s (%s) by %s (%s), name %s", old.Path, old.Scope, path, skill.Scope, skill.Name))
		}
		selected[skill.Name] = skill
	}
}

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func readSkill(path string) (PreparedSkill, error) {
	out := PreparedSkill{Path: path}
	text, err := readTextFile("", path)
	if err != nil {
		return out, err
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return out, fmt.Errorf("missing YAML frontmatter")
	}
	end := 1
	for end < len(lines) && strings.TrimSpace(lines[end]) != "---" {
		end++
	}
	if end == len(lines) {
		return out, fmt.Errorf("unterminated YAML frontmatter")
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Disable     bool   `yaml:"disable-model-invocation"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &meta); err != nil {
		return out, err
	}
	if meta.Disable {
		return out, fmt.Errorf("model invocation disabled (skill commands are not supported)")
	}
	if len(meta.Name) > 64 || !skillName.MatchString(meta.Name) {
		return out, fmt.Errorf("invalid skill name")
	}
	if strings.TrimSpace(meta.Description) == "" || len([]rune(meta.Description)) > 1024 {
		return out, fmt.Errorf("description must contain 1–1024 characters")
	}
	out.Name, out.Description = meta.Name, meta.Description
	return out, nil
}

func catalogInput(catalog string) []json.RawMessage {
	if catalog == "" {
		return nil
	}
	return []json.RawMessage{json.RawMessage(mustJSON(map[string]any{"role": "developer", "content": catalog}))}
}
