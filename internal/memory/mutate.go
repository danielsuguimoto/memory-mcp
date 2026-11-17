package memory

import (
	"bytes"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var gitRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Mutation struct {
	Kind             string
	Name             string
	NewName          string
	ExpectedRevision string
	Description      string
	Body             string
	Keywords         []string
}

type Change struct {
	Files    map[string][]byte
	Affected []string
	Revision string
}

func (m Mutation) Validate() error {
	switch m.Kind {
	case "create", "update", "rename", "delete":
		if err := ValidateName(m.Name); err != nil {
			return err
		}
	case "repair":
		return nil
	default:
		return Fail("INVALID_INPUT", "unknown mutation")
	}
	if m.Kind == "update" || m.Kind == "rename" || m.Kind == "delete" {
		if !gitRevision.MatchString(m.ExpectedRevision) {
			return Fail("INVALID_INPUT", "expected revision must be a 40-character Git blob ID")
		}
	}
	if m.Kind == "create" || m.Kind == "update" {
		if err := validateContent(m.Description, m.Body, m.Keywords); err != nil {
			return err
		}
	}
	if m.Kind == "rename" {
		return ValidateName(m.NewName)
	}
	return nil
}

func (s *State) Apply(m Mutation) (Change, error) {
	change := Change{Files: make(map[string][]byte)}
	if err := s.CheckIndexOwnership(); err != nil {
		return change, err
	}
	if m.Kind != "repair" {
		if err := ValidateName(m.Name); err != nil {
			return change, err
		}
	}
	put := func(name string, raw []byte) error {
		entry, err := Decode(name, raw)
		if err != nil {
			return err
		}
		s.Entries[name] = entry
		change.Files["memories/"+name+".md"] = raw
		change.Affected = append(change.Affected, name)
		return nil
	}
	switch m.Kind {
	case "create":
		if _, exists := s.Entries[m.Name]; exists {
			return change, Fail("ALREADY_EXISTS", "memory already exists: "+m.Name)
		}
		raw, err := Encode(m.Description, m.Body, m.Keywords)
		if err != nil {
			return change, err
		}
		if err := put(m.Name, raw); err != nil {
			return change, err
		}
		change.Revision = Revision(raw)
	case "update":
		if m.ExpectedRevision == "" {
			return change, Fail("INVALID_INPUT", "expected revision is required")
		}
		old, err := s.Get(m.Name, m.ExpectedRevision)
		if err != nil {
			return change, err
		}
		keywords := m.Keywords
		if keywords == nil {
			keywords = old.Keywords
		}
		raw, err := Encode(m.Description, m.Body, keywords)
		if err != nil {
			return change, err
		}
		change.Revision = Revision(raw)
		if old.Description != m.Description || old.Body != m.Body || !slices.Equal(old.Keywords, keywords) {
			if err := put(m.Name, raw); err != nil {
				return change, err
			}
		} else {
			change.Revision = old.Revision
			return change, nil
		}
	case "rename":
		if m.ExpectedRevision == "" {
			return change, Fail("INVALID_INPUT", "expected revision is required")
		}
		old, err := s.Get(m.Name, m.ExpectedRevision)
		if err != nil {
			return change, err
		}
		if err := ValidateName(m.NewName); err != nil {
			return change, err
		}
		if _, exists := s.Entries[m.NewName]; exists {
			return change, Fail("ALREADY_EXISTS", "memory already exists: "+m.NewName)
		}
		delete(s.Entries, m.Name)
		change.Files["memories/"+m.Name+".md"] = nil
		change.Affected = append(change.Affected, m.Name)
		old.Name = m.NewName
		s.Entries[m.NewName] = old
		from, to := "`mem:"+m.Name+"`", "`mem:"+m.NewName+"`"
		for _, name := range s.Names() {
			entry := s.Entries[name]
			desc := strings.ReplaceAll(entry.Description, from, to)
			body := strings.ReplaceAll(entry.Body, from, to)
			if name == m.NewName || desc != entry.Description || body != entry.Body {
				raw := entry.Raw
				if desc != entry.Description || body != entry.Body {
					raw, err = Encode(desc, body, entry.Keywords)
					if err != nil {
						return change, err
					}
				}
				if err := put(name, raw); err != nil {
					return change, err
				}
				if name == m.NewName {
					change.Revision = Revision(raw)
				}
			}
		}
	case "delete":
		if m.ExpectedRevision == "" {
			return change, Fail("INVALID_INPUT", "expected revision is required")
		}
		if _, err := s.Get(m.Name, m.ExpectedRevision); err != nil {
			return change, err
		}
		if refs := s.Referrers(m.Name); len(refs) > 0 {
			return change, Fail("REFERENCED", "memory referenced by: "+strings.Join(refs, ", "))
		}
		delete(s.Entries, m.Name)
		change.Files["memories/"+m.Name+".md"] = nil
		change.Affected = append(change.Affected, m.Name)
	case "repair":
	default:
		return change, Fail("INVALID_INPUT", "unknown mutation")
	}
	generated := s.GeneratedIndex()
	if len(generated) > MaxIndex {
		return Change{}, Fail("INDEX_TOO_LARGE", "generated index exceeds 4 MiB")
	}
	if !s.IndexExists || !bytes.Equal(s.Index, generated) {
		change.Files[IndexPath] = generated
	}
	sort.Strings(change.Affected)
	return change, nil
}
