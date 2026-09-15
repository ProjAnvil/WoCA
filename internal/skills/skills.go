// Package skills loads agent skills (SKILL.md files with YAML frontmatter)
// from a skills directory.
package skills

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/projanvil/woca/internal/telemetry"
)

// Skill is a loaded skill: a name, a one-line description, and the markdown
// body that is expanded into the prompt when the skill is invoked.
type Skill struct {
	Name        string
	Description string
	Body        string
	Args        []SkillArg
	// Source is the absolute path of the SKILL.md file (layered-merge
	// conflict logs; empty for hand-constructed values).
	Source string
}

// SkillArg is one declared argument for a skill.
type SkillArg struct {
	Name        string
	Description string
}

// Load walks root and loads every SKILL.md, using the containing directory
// name as the default skill name. Directory symlinks are followed (skills
// imported from ~/.claude / cc-switch are linked into ~/.woca/skills).
func Load(root string) ([]Skill, error) {
	var out []Skill
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // skip unreadable entries
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			info, err := os.Stat(path) // follows symlinks
			if err != nil {
				continue
			}
			switch {
			case info.IsDir():
				walk(path)
			case e.Name() == "SKILL.md":
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				sk := parseSkill(filepath.Base(dir), data)
				if abs, err := filepath.Abs(path); err == nil {
					sk.Source = abs
				} else {
					sk.Source = path
				}
				out = append(out, sk)
			}
		}
	}
	walk(root)
	slices.SortFunc(out, func(a, b Skill) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// LoadFS loads skills from a single-level filesystem tree (the embedded
// built-in skills): each child directory's SKILL.md becomes a skill, using
// the directory name as the default skill name. Source carries the
// "builtin/<name>" marker for layered-merge conflict logs (the body is fully
// in memory, so no on-disk path is needed at invocation time).
func LoadFS(fsys fs.FS) ([]Skill, error) {
	var out []Skill
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name()+"/SKILL.md")
		if err != nil {
			continue
		}
		sk := parseSkill(e.Name(), data)
		sk.Source = "builtin/" + sk.Name
		out = append(out, sk)
	}
	slices.SortFunc(out, func(a, b Skill) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Find returns the skill with the given name.
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Expand substitutes {name} placeholders in the skill body with the provided
// argument values. Unknown placeholders are left as-is.
func Expand(s Skill, args map[string]any) string {
	body := s.Body
	for name, value := range args {
		body = strings.ReplaceAll(body, "{"+name+"}", fmt.Sprint(value))
	}
	return body
}

// Loader re-loads skills from a root directory on demand (hot reload).
type Loader struct {
	root string
}

// NewLoader returns a Loader that reads skills from root on every Load call.
func NewLoader(root string) *Loader {
	return &Loader{root: root}
}

// Load (re-)reads skills from the loader's root directory from disk each call.
func (l *Loader) Load() ([]Skill, error) {
	sp := telemetry.Start(context.Background(), "load_skills", telemetry.KindSkill).
		Set(telemetry.AttrOperationName, "load_skill")
	skills, err := Load(l.root)
	if err == nil {
		sp.Set("woca.skill.count", len(skills))
	}
	sp.End(context.Background(), err)
	return skills, err
}

// parseSkill parses a SKILL.md with optional YAML frontmatter delimited by
// leading "---" lines.
func parseSkill(defaultName string, data []byte) Skill {
	text := strings.TrimSpace(string(data))
	s := Skill{Name: defaultName}

	if strings.HasPrefix(text, "---") {
		parts := strings.SplitN(text, "---", 3)
		if len(parts) == 3 {
			lines := strings.Split(parts[1], "\n")
			for i := range lines {
				line := strings.TrimSpace(lines[i])
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "name":
					s.Name = strings.TrimSpace(v)
				case "description":
					s.Description = strings.TrimSpace(v)
				case "args":
					if strings.TrimSpace(v) != "" {
						s.Args = parseArgsString(v)
					} else {
						s.Args = parseArgsList(lines, i+1)
					}
				}
			}
			s.Body = strings.TrimSpace(parts[2])
		} else {
			s.Body = text
		}
	} else {
		s.Body = text
	}
	return s
}

// parseArgsString parses a comma-separated list of argument names.
func parseArgsString(value string) []SkillArg {
	var out []SkillArg
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, SkillArg{Name: part})
	}
	return out
}

// parseArgsList parses a YAML list of "name: description" argument lines.
func parseArgsList(lines []string, start int) []SkillArg {
	var out []SkillArg
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "-") {
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(line, "-"))
		name, desc, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		out = append(out, SkillArg{
			Name:        strings.TrimSpace(name),
			Description: strings.TrimSpace(desc),
		})
	}
	return out
}
