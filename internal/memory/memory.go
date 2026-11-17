package memory

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	IndexPath      = "MEMORY.md"
	IndexMarker    = "<!-- memory-mcp:index:v1 -->"
	MaxName        = 240
	MaxDescription = 240
	MaxKeywords    = 16
	MaxKeyword     = 64
	MaxBody        = 1 << 20
	MaxIndex       = 4 << 20
	MaxResults     = 100
)

var segment = regexp.MustCompile(`^[a-z0-9_-]+$`)

type Error struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	CurrentRevision string `json:"current_revision,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Fail(code, message string) error { return &Error{Code: code, Message: message} }

type Entry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Keywords    []string `json:"keywords,omitempty"`
	Body        string   `json:"body"`
	Revision    string   `json:"revision"`
	Raw         []byte   `json:"-"`
}

type State struct {
	Entries     map[string]Entry
	Index       []byte
	IndexExists bool
}

type frontMatter struct {
	Description string   `yaml:"description"`
	Keywords    []string `yaml:"keywords,omitempty"`
}

func ValidateName(name string) error {
	if len(name) == 0 || len(name) > MaxName {
		return Fail("INVALID_INPUT", "invalid memory name length")
	}
	for _, part := range strings.Split(name, "/") {
		if !segment.MatchString(part) {
			return Fail("INVALID_INPUT", "memory names must use lowercase letters, digits, hyphens, and underscores")
		}
	}
	return nil
}

func validateContent(description, body string, keywords []string) error {
	if !utf8.ValidString(description) || len(description) == 0 || len(description) > MaxDescription || strings.TrimSpace(description) != description || strings.ContainsAny(description, "\r\n\x00") {
		return Fail("INVALID_INPUT", "description must be a nonempty single line of at most 240 bytes")
	}
	if !utf8.ValidString(body) || len(body) > MaxBody || strings.ContainsRune(body, 0) {
		return Fail("INVALID_INPUT", "body must be UTF-8 and at most 1 MiB")
	}
	if len(keywords) > MaxKeywords {
		return Fail("INVALID_INPUT", "too many keywords")
	}
	for _, keyword := range keywords {
		if !utf8.ValidString(keyword) || len(keyword) == 0 || len(keyword) > MaxKeyword || strings.TrimSpace(keyword) != keyword || strings.IndexFunc(keyword, unicode.IsControl) >= 0 {
			return Fail("INVALID_INPUT", "keywords must be nonempty single lines of at most 64 bytes")
		}
	}
	return nil
}

func Encode(description, body string, keywords []string) ([]byte, error) {
	if err := validateContent(description, body, keywords); err != nil {
		return nil, err
	}
	meta, err := yaml.Marshal(frontMatter{Description: description, Keywords: keywords})
	if err != nil {
		return nil, err
	}
	return []byte("---\n" + string(meta) + "---\n" + body), nil
}

func Revision(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func Decode(name string, data []byte) (Entry, error) {
	if err := ValidateName(name); err != nil {
		return Entry{}, err
	}
	if !utf8.Valid(data) || len(data) > MaxBody+4096 || !bytes.HasPrefix(data, []byte("---\n")) {
		return Entry{}, Fail("INVALID_MEMORY", "invalid memory file: "+name)
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 {
		return Entry{}, Fail("INVALID_MEMORY", "missing front matter delimiter: "+name)
	}
	metaBytes := data[4 : 4+end]
	body := string(data[4+end+5:])
	dec := yaml.NewDecoder(bytes.NewReader(metaBytes))
	dec.KnownFields(true)
	var meta frontMatter
	if err := dec.Decode(&meta); err != nil {
		return Entry{}, Fail("INVALID_MEMORY", "invalid front matter: "+name)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Entry{}, Fail("INVALID_MEMORY", "invalid front matter: "+name)
	}
	if err := validateContent(meta.Description, body, meta.Keywords); err != nil {
		return Entry{}, Fail("INVALID_MEMORY", "invalid memory content: "+name)
	}
	return Entry{Name: name, Description: meta.Description, Keywords: meta.Keywords, Body: body, Revision: Revision(data), Raw: data}, nil
}

func Load(files map[string][]byte) (*State, error) {
	s := &State{Entries: make(map[string]Entry)}
	for path, data := range files {
		if path == IndexPath {
			s.IndexExists = true
			s.Index = data
			continue
		}
		if !strings.HasPrefix(path, "memories/") {
			continue
		}
		if !strings.HasSuffix(path, ".md") {
			return nil, Fail("INVALID_MEMORY", "unexpected managed file: "+path)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(path, "memories/"), ".md")
		entry, err := Decode(name, data)
		if err != nil {
			return nil, err
		}
		s.Entries[name] = entry
	}
	return s, nil
}

func (s *State) Names() []string {
	names := make([]string, 0, len(s.Entries))
	for name := range s.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *State) GeneratedIndex() []byte {
	var b strings.Builder
	b.WriteString(IndexMarker + "\n# Memory index\n\n")
	for _, name := range s.Names() {
		d := html.EscapeString(strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "{", "\\{", "}", "\\}", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "+", "\\+", "-", "\\-", ".", "\\.", "!", "\\!", "|", "\\|").Replace(s.Entries[name].Description))
		fmt.Fprintf(&b, "- `mem:%s` - %s\n", name, d)
	}
	return []byte(b.String())
}

func (s *State) IndexStatus() string {
	if !s.IndexExists {
		return "missing"
	}
	if bytes.Equal(s.Index, s.GeneratedIndex()) {
		return "current"
	}
	return "stale"
}

func (s *State) CheckIndexOwnership() error {
	if s.IndexExists && !bytes.HasPrefix(s.Index, []byte(IndexMarker+"\n")) {
		return Fail("INDEX_OWNERSHIP", "MEMORY.md is not a supported generated index")
	}
	return nil
}

func (s *State) Get(name, revision string) (Entry, error) {
	if err := ValidateName(name); err != nil {
		return Entry{}, err
	}
	e, ok := s.Entries[name]
	if !ok {
		return Entry{}, Fail("NOT_FOUND", "memory not found: "+name)
	}
	if revision != "" && revision != e.Revision {
		return Entry{}, &Error{Code: "REVISION_CONFLICT", Message: "memory changed: " + name, CurrentRevision: e.Revision}
	}
	return e, nil
}

func (s *State) Referrers(name string) []string {
	ref := "`mem:" + name + "`"
	var result []string
	for _, n := range s.Names() {
		if n != name && (strings.Contains(s.Entries[n].Body, ref) || strings.Contains(s.Entries[n].Description, ref)) {
			result = append(result, n)
		}
	}
	return result
}
