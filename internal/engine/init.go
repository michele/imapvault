// Package engine orchestrates repository operations: init, check (and later
// backup, prune and restore). The CLI calls only into this package; it never
// touches the repository or the server directly.
package engine

import (
	"fmt"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/repo"
)

// InitResult describes a repository that Init just created.
type InitResult struct {
	Path          string `json:"path"`
	RepoID        string `json:"repo_id"`
	Account       string `json:"account"`
	FormatVersion int    `json:"format_version"`
	SchemaVersion int    `json:"schema_version"`
}

// accountOf is the binding a config implies. Only host and username take part,
// so changing the password source never invalidates a repository.
func accountOf(cfg *config.Config) repo.Account {
	return repo.Account{ID: config.AccountID(cfg), Host: cfg.IMAP.Host, Username: cfg.IMAP.Username}
}

// Init creates the repository at cfg.Repository.Path and its empty index. It
// never contacts the server, so it needs no password.
func Init(cfg *config.Config) (*InitResult, error) {
	r, err := repo.Init(cfg.Repository.Path, accountOf(cfg))
	if err != nil {
		return nil, err
	}
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		// back out, so the directory is not left as a repository without an index.
		_ = r.Uninit()
		return nil, fmt.Errorf("create index: %w", err)
	}
	v, verr := db.SchemaVersion()
	if cerr := db.Close(); cerr != nil && verr == nil {
		verr = cerr
	}
	if verr != nil {
		_ = r.Uninit()
		return nil, fmt.Errorf("create index: %w", verr)
	}
	return &InitResult{
		Path:          r.Path,
		RepoID:        r.Meta.ID,
		Account:       r.Meta.Account.ID,
		FormatVersion: r.Meta.FormatVersion,
		SchemaVersion: v,
	}, nil
}
