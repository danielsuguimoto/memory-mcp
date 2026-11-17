package config

import "testing"

func TestValidateRemote(t *testing.T) {
	for _, remote := range []string{
		"http://example.test/repo.git",
		"file:///tmp/repo",
		"https://user:secret@example.test/repo.git",
		"https://example.test/repo.git?token=secret",
		"https://example.test/repo.git?",
	} {
		c := Config{URL: remote, Branch: "main", AuthorName: "Memory", AuthorEmail: "memory@localhost"}
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted invalid remote %q", remote)
		}
	}
	c := Config{URL: "https://example.test/repo.git", Branch: "main", AuthorName: "Memory", AuthorEmail: "memory@localhost"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Branch = "main..other"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted invalid branch")
	}
}
