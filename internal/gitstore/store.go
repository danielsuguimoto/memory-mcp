package gitstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitmemory "github.com/go-git/go-git/v5/storage/memory"

	"memory-mcp/internal/config"
	"memory-mcp/internal/memory"
)

type Credentials func(context.Context) (transport.AuthMethod, error)

type Store struct {
	Config      config.Config
	Credentials Credentials
	slots       chan struct{}
}

type Snapshot struct {
	Commit string
	State  *memory.State
	repo   *git.Repository
	fs     billy.Filesystem
}

type Result struct {
	Commit   string   `json:"commit"`
	Revision string   `json:"revision,omitempty"`
	Affected []string `json:"affected,omitempty"`
	Status   string   `json:"status"`
}

func ConfigureHTTP() {
	c := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host)) {
				return errors.New("cross-origin Git redirect rejected")
			}
			return nil
		},
	}
	client.InstallProtocol("https", githttp.NewClient(c))
}

func New(c config.Config, creds Credentials) *Store {
	return &Store{Config: c, Credentials: creds, slots: make(chan struct{}, 4)}
}

func (s *Store) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return memory.Fail("CANCELLED", "operation cancelled")
	}
}

func (s *Store) Snapshot(ctx context.Context) (*Snapshot, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-s.slots }()
	return s.snapshot(ctx, 1)
}

func (s *Store) snapshot(ctx context.Context, depth int) (*Snapshot, error) {
	auth, err := s.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	fs := memfs.New()
	repo, err := git.CloneContext(ctx, gitmemory.NewStorage(), fs, &git.CloneOptions{
		URL:           s.Config.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(s.Config.Branch),
		SingleBranch:  true,
		Depth:         depth,
		Tags:          git.NoTags,
	})
	if err != nil {
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
			return nil, memory.Fail("AUTH_REQUIRED", "remote authentication or permission is required")
		}
		return nil, memory.Fail("REMOTE_ERROR", "could not fetch the configured Git branch")
	}
	head, err := repo.Head()
	if err != nil {
		return nil, memory.Fail("REMOTE_ERROR", "configured branch has no commit")
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, memory.Fail("REMOTE_ERROR", "could not read branch commit")
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, memory.Fail("REMOTE_ERROR", "could not read branch tree")
	}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, walkErr := walker.Next()
		if errors.Is(walkErr, io.EOF) {
			break
		}
		if walkErr != nil {
			return nil, memory.Fail("INVALID_MEMORY", "could not validate branch tree")
		}
		if name == "memories" && entry.Mode != filemode.Dir {
			return nil, memory.Fail("INVALID_MEMORY", "memories must be a directory")
		}
		if strings.HasPrefix(name, "memories/") {
			if entry.Mode == filemode.Submodule || entry.Mode == filemode.Symlink {
				return nil, memory.Fail("INVALID_MEMORY", "managed path is not a regular file: "+name)
			}
			if entry.Mode == filemode.Dir {
				if err := memory.ValidateName(strings.TrimPrefix(name, "memories/")); err != nil {
					return nil, memory.Fail("INVALID_MEMORY", "invalid managed directory: "+name)
				}
			}
		}
	}
	files := map[string][]byte{}
	err = tree.Files().ForEach(func(f *object.File) error {
		if f.Name != memory.IndexPath && f.Name != "memories" && !strings.HasPrefix(f.Name, "memories/") {
			return nil
		}
		if f.Name == "memories" {
			return memory.Fail("INVALID_MEMORY", "memories must be a directory")
		}
		if f.Mode != filemode.Regular && f.Mode != filemode.Executable && f.Mode != filemode.Deprecated {
			return memory.Fail("INVALID_MEMORY", "managed path is not a regular file: "+f.Name)
		}
		if f.Size > memory.MaxBody+4096 && f.Name != memory.IndexPath || f.Name == memory.IndexPath && f.Size > memory.MaxIndex {
			return memory.Fail("INVALID_MEMORY", "managed file exceeds its size limit: "+f.Name)
		}
		r, err := f.Reader()
		if err != nil {
			return memory.Fail("REMOTE_ERROR", "could not read managed file")
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return memory.Fail("REMOTE_ERROR", "could not read managed file")
		}
		files[f.Name] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	state, err := memory.Load(files)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Commit: head.Hash().String(), State: state, repo: repo, fs: fs}, nil
}

func (s *Store) Mutate(ctx context.Context, mutation memory.Mutation) (Result, error) {
	if err := mutation.Validate(); err != nil {
		return Result{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer func() { <-s.slots }()
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.snapshot(ctx, 1)
		if err != nil {
			return Result{}, err
		}
		change, err := snap.State.Apply(mutation)
		if err != nil {
			return Result{}, err
		}
		if len(change.Files) == 0 {
			return Result{Commit: snap.Commit, Revision: change.Revision, Status: "no_change"}, nil
		}
		wt, err := snap.repo.Worktree()
		if err != nil {
			return Result{}, memory.Fail("REMOTE_ERROR", "could not prepare in-memory worktree")
		}
		paths := make([]string, 0, len(change.Files))
		for p := range change.Files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			data := change.Files[p]
			if data == nil {
				if _, err := wt.Remove(p); err != nil {
					return Result{}, memory.Fail("REMOTE_ERROR", "could not remove memory from in-memory tree")
				}
				continue
			}
			if err := snap.fs.MkdirAll(path.Dir(p), 0755); err != nil {
				return Result{}, memory.Fail("INVALID_MEMORY", "managed path conflicts with an existing file")
			}
			f, err := snap.fs.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return Result{}, memory.Fail("REMOTE_ERROR", "could not update in-memory tree")
			}
			_, writeErr := f.Write(data)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				return Result{}, memory.Fail("REMOTE_ERROR", "could not update in-memory tree")
			}
			if _, err := wt.Add(p); err != nil {
				return Result{}, memory.Fail("REMOTE_ERROR", "could not stage in-memory changes")
			}
		}
		now := time.Now()
		identity := &object.Signature{Name: s.Config.AuthorName, Email: s.Config.AuthorEmail, When: now}
		hash, err := wt.Commit("memory: "+mutation.Kind, &git.CommitOptions{Author: identity, Committer: identity})
		if err != nil {
			return Result{}, memory.Fail("REMOTE_ERROR", "could not create in-memory commit")
		}
		auth, err := s.Credentials(ctx)
		if err != nil {
			return Result{}, err
		}
		ref := plumbing.NewBranchReferenceName(s.Config.Branch).String()
		err = snap.repo.PushContext(ctx, &git.PushOptions{
			Auth:              auth,
			RefSpecs:          []gitconfig.RefSpec{gitconfig.RefSpec(ref + ":" + ref)},
			RequireRemoteRefs: []gitconfig.RefSpec{gitconfig.RefSpec(snap.Commit + ":" + ref)},
		})
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			return Result{Commit: hash.String(), Revision: change.Revision, Affected: change.Affected, Status: "pushed"}, nil
		}
		msg := err.Error()
		if strings.Contains(msg, "required to be") || strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "reference has changed") || strings.Contains(msg, "command error on "+ref+": incorrect old value provided") {
			continue
		}
		if strings.Contains(msg, "command error on "+ref+": failed to update ref") {
			latest, fetchErr := s.snapshot(ctx, 1)
			if fetchErr == nil && latest.Commit != snap.Commit {
				continue
			}
			return Result{}, memory.Fail("REMOTE_REJECTED", "remote rejected the branch update")
		}
		if strings.Contains(msg, "command error on "+ref+":") {
			return Result{}, memory.Fail("REMOTE_REJECTED", "remote rejected the branch update")
		}
		if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
			return Result{}, memory.Fail("AUTH_REQUIRED", "remote rejected Git credentials or permissions")
		}
		if ok, _ := s.contains(ctx, hash.String()); ok {
			return Result{Commit: hash.String(), Revision: change.Revision, Affected: change.Affected, Status: "pushed"}, nil
		}
		return Result{}, &memory.Error{Code: "OUTCOME_UNKNOWN", Message: "push outcome could not be confirmed; candidate commit: " + hash.String()}
	}
	return Result{}, memory.Fail("REMOTE_REJECTED", "branch changed repeatedly; retry after reading current memory")
}

func (s *Store) CheckCommit(ctx context.Context, hash string) (bool, error) {
	if len(hash) != 40 || plumbing.NewHash(hash).String() != strings.ToLower(hash) {
		return false, memory.Fail("INVALID_INPUT", "commit ID must be a 40-character Git hash")
	}
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer func() { <-s.slots }()
	return s.contains(ctx, hash)
}

func (s *Store) contains(ctx context.Context, hash string) (bool, error) {
	snap, err := s.snapshot(ctx, 64)
	if err != nil {
		return false, err
	}
	iter, err := snap.repo.Log(&git.LogOptions{From: plumbing.NewHash(snap.Commit)})
	if err != nil {
		return false, nil
	}
	defer iter.Close()
	count := 0
	found := false
	_ = iter.ForEach(func(c *object.Commit) error {
		count++
		if c.Hash.String() == hash {
			found = true
			return io.EOF
		}
		if count >= 64 {
			return io.EOF
		}
		return nil
	})
	return found, nil
}
