package memory

import (
	"slices"
	"strings"
	"testing"
)

func TestKeywordsPersistAcrossUpdatesAndRenames(t *testing.T) {
	s := &State{Entries: map[string]Entry{}}
	created, err := s.Apply(Mutation{Kind: "create", Name: "storage", Description: "Storage", Body: "body", Keywords: []string{"offline", "empty repository"}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.Apply(Mutation{Kind: "update", Name: "storage", ExpectedRevision: created.Revision, Description: "Storage", Body: "updated"})
	if err != nil || !slices.Equal(s.Entries["storage"].Keywords, []string{"offline", "empty repository"}) {
		t.Fatalf("omitted keywords were lost: %+v %v", s.Entries["storage"], err)
	}
	if _, err := s.Apply(Mutation{Kind: "create", Name: "links", Description: "Links", Body: "`mem:storage`", Keywords: []string{"references"}}); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.Apply(Mutation{Kind: "rename", Name: "storage", NewName: "renamed", ExpectedRevision: updated.Revision})
	if err != nil || !slices.Equal(s.Entries["renamed"].Keywords, []string{"offline", "empty repository"}) {
		t.Fatalf("rename lost keywords: %+v %v", s.Entries["renamed"], err)
	}
	if !slices.Equal(s.Entries["links"].Keywords, []string{"references"}) || !strings.Contains(s.Entries["links"].Body, "`mem:renamed`") {
		t.Fatal("reference rewrite lost keywords")
	}
	cleared, err := s.Apply(Mutation{Kind: "update", Name: "renamed", ExpectedRevision: renamed.Revision, Description: "Storage", Body: "updated", Keywords: []string{}})
	if err != nil || len(s.Entries["renamed"].Keywords) != 0 || cleared.Revision == renamed.Revision {
		t.Fatalf("empty keywords did not clear metadata: %+v %v", s.Entries["renamed"], err)
	}
}

func TestInvalidKeywords(t *testing.T) {
	for _, keywords := range [][]string{{""}, {" leading"}, {"line\nbreak"}, {strings.Repeat("a", MaxKeyword+1)}, make([]string, MaxKeywords+1)} {
		if _, err := Encode("Description", "body", keywords); err == nil {
			t.Fatalf("accepted invalid keywords: %q", keywords)
		}
	}
	if _, err := Decode("old", []byte("---\ndescription: Old\n---\nbody")); err != nil {
		t.Fatalf("rejected legacy memory: %v", err)
	}
	if _, err := Decode("bad", []byte("---\ndescription: Bad\nkeywords: not-a-list\n---\nbody")); err == nil {
		t.Fatal("accepted invalid keyword metadata")
	}
}

func TestLifecycleAndReferences(t *testing.T) {
	s := &State{Entries: map[string]Entry{}}
	created, err := s.Apply(Mutation{Kind: "create", Name: "project/auth", Description: "Login", Body: "Details"})
	if err != nil || created.Revision == "" {
		t.Fatalf("create: %#v %v", created, err)
	}
	s.Index, s.IndexExists = created.Files[IndexPath], true
	_, err = s.Apply(Mutation{Kind: "create", Name: "other", Description: "Reference", Body: "See `mem:project/auth`."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(Mutation{Kind: "delete", Name: "project/auth", ExpectedRevision: created.Revision}); err == nil || !strings.Contains(err.Error(), "REFERENCED") {
		t.Fatalf("expected reference protection, got %v", err)
	}
	if _, err := s.Apply(Mutation{Kind: "update", Name: "project/auth", ExpectedRevision: "stale", Description: "Login", Body: "Changed"}); err == nil || !strings.Contains(err.Error(), "REVISION_CONFLICT") {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	renamed, err := s.Apply(Mutation{Kind: "rename", Name: "project/auth", NewName: "project/signin", ExpectedRevision: created.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(renamed.Files["memories/other.md"]), "`mem:project/signin`") {
		t.Fatal("reference was not updated")
	}
	if !strings.Contains(string(renamed.Files[IndexPath]), "`mem:project/signin`") {
		t.Fatal("index was not updated")
	}
}

func TestValidationAndIndexOwnership(t *testing.T) {
	for _, name := range []string{"", "../bad", "/absolute", "a//b", "Upper", "a.md", `a\b`} {
		if ValidateName(name) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, raw := range []string{"---\ndescription: [\n---\nbody", "---\ndescription: ok\nunknown: value\n---\nbody", "not front matter"} {
		if _, err := Decode("example", []byte(raw)); err == nil {
			t.Fatalf("accepted malformed content %q", raw)
		}
	}
	s := &State{Entries: map[string]Entry{}, Index: []byte("# User index\n"), IndexExists: true}
	if _, err := s.Apply(Mutation{Kind: "repair"}); err == nil {
		t.Fatal("overwrote an unmanaged index")
	}
}

func TestUnchangedUpdateDoesNotRepairIndex(t *testing.T) {
	s := &State{Entries: map[string]Entry{}}
	created, err := s.Apply(Mutation{Kind: "create", Name: "example", Description: "Example", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	s.Index = []byte(IndexMarker + "\nold index\n")
	s.IndexExists = true
	change, err := s.Apply(Mutation{Kind: "update", Name: "example", ExpectedRevision: created.Revision, Description: "Example", Body: "body"})
	if err != nil || len(change.Files) != 0 || change.Revision != created.Revision {
		t.Fatalf("unchanged update: %+v %v", change, err)
	}
}
