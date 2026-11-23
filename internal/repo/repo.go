// Package repo owns the on-disk repository: its directory layout, repo.toml
// (format version, repository id, account binding) and the repository lock.
// It knows nothing about blobs or the index; it only says where they live.
//
//	<repo>/
//	  repo.toml       format_version, id, created_at, account binding
//	  objects/ab/…    blob store (package objects)
//	  index.db        SQLite index (package index)
//	  index-backups/  VACUUM INTO copies
//	  lock            flock target; holder details while a writer runs
//	  tmp/            staging for atomic writes
package repo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// FormatVersion is the on-disk format this binary reads and writes. Any change
// to the layout, the blob envelope or the index schema bumps it and ships a
// migration, or a clear refusal to open the old or new repository.
const FormatVersion = 1

const metaFile = "repo.toml"

var (
	// ErrNotARepo means the directory has no repo.toml.
	ErrNotARepo = errors.New("not an imapvault repository")
	// ErrExists means the directory is already a repository.
	ErrExists = errors.New("repository already exists")
	// ErrFormatVersion means the repository was written by a different format version.
	ErrFormatVersion = errors.New("unsupported repository format version")
	// ErrWrongAccount means the repository is bound to a different account.
	ErrWrongAccount = errors.New("repository belongs to a different account")
)

// Account is the mailbox a repository is bound to at init time.
type Account struct {
	ID       string `toml:"id"` // canonical "user@host", compared on every open
	Host     string `toml:"host"`
	Username string `toml:"username"`
}

// Meta is the content of repo.toml.
type Meta struct {
	FormatVersion int       `toml:"format_version"`
	ID            string    `toml:"id"`
	CreatedAt     time.Time `toml:"created_at"`
	Account       Account   `toml:"account"`
}

// Repo is an opened repository directory.
type Repo struct {
	Path string
	Meta Meta
}

// ObjectsDir is the blob store root.
func (r *Repo) ObjectsDir() string { return filepath.Join(r.Path, "objects") }

// TmpDir is the staging area for atomic writes.
func (r *Repo) TmpDir() string { return filepath.Join(r.Path, "tmp") }

// IndexPath is the SQLite index file.
func (r *Repo) IndexPath() string { return filepath.Join(r.Path, "index.db") }

// IndexBackupsDir holds VACUUM INTO copies of the index.
func (r *Repo) IndexBackupsDir() string { return filepath.Join(r.Path, "index-backups") }

// Init creates a new repository at path, bound to acct. The path must be
// absolute and either missing or an empty directory.
func Init(path string, acct Account) (*Repo, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("repository path %q must be absolute", path)
	}
	if _, err := os.Stat(filepath.Join(path, metaFile)); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, path)
	}
	if ents, err := os.ReadDir(path); err == nil && len(ents) > 0 {
		return nil, fmt.Errorf("directory %s is not empty; refusing to initialise a repository there", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	r := &Repo{Path: path, Meta: Meta{
		FormatVersion: FormatVersion,
		ID:            hex.EncodeToString(id[:]),
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		Account:       acct,
	}}
	for _, d := range []string{r.Path, r.ObjectsDir(), r.TmpDir(), r.IndexBackupsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	// repo.toml goes last: its presence is what makes the directory a repository,
	// so an interrupted init leaves a directory that can simply be re-initialised.
	if err := r.writeMeta(); err != nil {
		return nil, err
	}
	return r, nil
}

// Open opens an existing repository. When acct is not nil the repository must be
// bound to that account. It refuses any format version other than FormatVersion.
func Open(path string, acct *Account) (*Repo, error) {
	data, err := os.ReadFile(filepath.Join(path, metaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s has no %s", ErrNotARepo, path, metaFile)
	}
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(path, metaFile), err)
	}
	switch {
	case m.FormatVersion > FormatVersion:
		return nil, fmt.Errorf("%w: repository is format %d, this imapvault reads format %d (created by a newer imapvault; upgrade)",
			ErrFormatVersion, m.FormatVersion, FormatVersion)
	case m.FormatVersion != FormatVersion:
		return nil, fmt.Errorf("%w: repository is format %d, this imapvault reads format %d (no migration available)",
			ErrFormatVersion, m.FormatVersion, FormatVersion)
	}
	if acct != nil && m.Account.ID != acct.ID {
		return nil, fmt.Errorf("%w: repository is bound to %s, config is %s", ErrWrongAccount, m.Account.ID, acct.ID)
	}
	return &Repo{Path: path, Meta: m}, nil
}

// writeMeta writes repo.toml atomically: temp file, fsync, rename, fsync dir.
func (r *Repo) writeMeta() error {
	data, err := toml.Marshal(r.Meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.TmpDir(), "repo.toml-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(r.Path, metaFile)); err != nil {
		return err
	}
	d, err := os.Open(r.Path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
