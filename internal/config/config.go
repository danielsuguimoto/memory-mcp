package config

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

type Config struct {
	URL, Branch, Username, Token, ClientID, AuthorName, AuthorEmail string
}

func Load() (Config, error) {
	c := Config{
		URL:         os.Getenv("MEMORY_GIT_URL"),
		Branch:      os.Getenv("MEMORY_GIT_BRANCH"),
		Username:    os.Getenv("MEMORY_GIT_USERNAME"),
		Token:       os.Getenv("MEMORY_GIT_TOKEN"),
		ClientID:    os.Getenv("MEMORY_GITHUB_CLIENT_ID"),
		AuthorName:  os.Getenv("MEMORY_GIT_AUTHOR_NAME"),
		AuthorEmail: os.Getenv("MEMORY_GIT_AUTHOR_EMAIL"),
	}
	if c.AuthorName == "" {
		c.AuthorName = "memory-mcp"
	}
	if c.AuthorEmail == "" {
		c.AuthorEmail = "memory-mcp@localhost"
	}
	if c.Username == "" {
		c.Username = "x-access-token"
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("MEMORY_GIT_URL must be an HTTPS URL without credentials, query, or fragment")
	}
	if c.Branch == "" || strings.HasPrefix(c.Branch, "-") || plumbing.NewBranchReferenceName(c.Branch).Validate() != nil {
		return errors.New("MEMORY_GIT_BRANCH must be a valid existing branch name")
	}
	for _, v := range []string{c.AuthorName, c.AuthorEmail, c.Username, c.ClientID} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("configuration values cannot contain control characters")
		}
	}
	if c.AuthorName == "" || c.AuthorEmail == "" || strings.ContainsAny(c.AuthorEmail, "<> ") || strings.ContainsAny(c.AuthorName, "<>") {
		return errors.New("invalid Git author identity")
	}
	return nil
}

func (c Config) GitHub() bool {
	u, err := url.Parse(c.URL)
	return err == nil && strings.EqualFold(u.Hostname(), "github.com")
}
