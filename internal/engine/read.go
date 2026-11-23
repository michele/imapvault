package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

// minHashPrefix is the shortest blob hash prefix `show` accepts.
const minHashPrefix = 8

// reader is a repository opened for reading under a shared lock.
type reader struct {
	r        *repo.Repo
	db       *index.DB
	opener   *seal.Opener
	identity string // identity text, "" for a plain repository
	names    bool   // sealed values can be opened (plain repository, or the key was resolved)
	warnings []string
	unlock   func()
}

func (rd *reader) close() {
	_ = rd.db.Close()
	rd.unlock()
}

// keyMode says what a command needs of the private key.
type keyMode int

const (
	keyNone        keyMode = iota // reads no sealed value: never resolves an identity
	keyRequired                   // reads sealed values: the key must resolve and match
	keyIfAvailable                // reads sealed values if it can: with no key configured, falls back to plaintext columns
)

// openReader opens the repository for reading. needKey says whether the command
// reads sealed values: if so and the repository is encrypted, the private key is
// resolved and proven against the canary first, so a wrong key is reported as
// such and not as corruption. A command that reads no sealed value (snapshots)
// never resolves an identity.
func openReader(ctx context.Context, cfg *config.Config, needKey bool) (*reader, error) {
	if needKey {
		return openReaderMode(ctx, cfg, keyRequired)
	}
	return openReaderMode(ctx, cfg, keyNone)
}

func openReaderMode(ctx context.Context, cfg *config.Config, mode keyMode) (*reader, error) {
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		return nil, err
	}
	rd := &reader{r: r, opener: seal.NewOpener(nil), names: !r.Meta.Encryption.Enabled}
	if mode != keyNone && r.Meta.Encryption.Enabled {
		warnings, err := cfg.ResolveIdentity(ctx) // may run identity_command: before the lock
		switch {
		case err != nil && mode == keyIfAvailable && errors.Is(err, config.ErrNoIdentity):
			rd.warnings = append(rd.warnings, "no private key is configured: sealed values (folder names, alert summaries) are not shown")
		case err != nil:
			return nil, err
		default:
			rd.warnings = warnings
			rd.identity = cfg.Encryption.Identity
			if rd.opener, err = openerOf(rd.identity); err != nil {
				return nil, err
			}
			if err := checkCanary(r, rd.opener); err != nil {
				return nil, err
			}
			rd.names = true
		}
	}
	if rd.unlock, err = r.Lock(repo.Shared); err != nil {
		return nil, err
	}
	if rd.db, err = index.Open(r.IndexPath(), index.Options{ReadOnly: true}); err != nil {
		rd.unlock()
		return nil, err
	}
	return rd, nil
}

// snapshotID resolves "latest" (or "") and a numeric id.
func (rd *reader) snapshotID(spec string) (int64, error) {
	if spec == "" || spec == "latest" {
		id, err := rd.db.LatestSnapshot()
		if err != nil {
			return 0, err
		}
		if id == 0 {
			return 0, errors.New("there are no snapshots yet: run backup first")
		}
		return id, nil
	}
	if rest, ok := strings.CutPrefix(spec, "latest~"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("snapshot %q: expected latest~N with N a number (latest~1 is the one before the latest)", spec)
		}
		snaps, err := rd.db.Snapshots()
		if err != nil {
			return 0, err
		}
		if n >= len(snaps) {
			return 0, fmt.Errorf("snapshot %q does not exist: the repository has %d snapshot(s)", spec, len(snaps))
		}
		return snaps[len(snaps)-1-n].ID, nil
	}
	id, err := strconv.ParseInt(spec, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("snapshot %q: expected \"latest\", \"latest~N\" or a snapshot number", spec)
	}
	ok, err := rd.db.HasSnapshot(id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("snapshot %d does not exist", id)
	}
	return id, nil
}

// SnapshotInfo is one line of `snapshots`.
type SnapshotInfo struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Folders   int       `json:"folders"`
	Messages  int       `json:"messages"`
	Pinned    bool      `json:"pinned"`
	Flagged   bool      `json:"flagged"`
	Forgotten bool      `json:"forgotten"`
}

// Snapshots lists snapshots, oldest first. It reads no sealed value, so it needs
// no private key.
func Snapshots(ctx context.Context, cfg *config.Config) ([]SnapshotInfo, error) {
	rd, err := openReader(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	defer rd.close()
	snaps, err := rd.db.Snapshots()
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, len(snaps))
	for i, s := range snaps {
		out[i] = SnapshotInfo{ID: s.ID, CreatedAt: s.CreatedAt, Folders: s.Folders, Messages: s.Messages, Pinned: s.Pinned, Flagged: s.Flagged, Forgotten: s.Forgotten}
	}
	return out, nil
}

// LsFolder is a folder in a snapshot.
type LsFolder struct {
	Name       string `json:"name"`
	SpecialUse string `json:"special_use,omitempty"`
	Messages   int    `json:"messages"`
}

// LsMessage is a message in a snapshot. Date is the server's INTERNALDATE.
type LsMessage struct {
	UID         uint32    `json:"uid"`
	UIDValidity uint32    `json:"uidvalidity"`
	Flags       string    `json:"flags"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Date        time.Time `json:"date,omitzero"`
	From        string    `json:"from,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	MessageID   string    `json:"message_id,omitempty"`
}

// LsResult is the folders of a snapshot, or the messages of one folder.
type LsResult struct {
	Snapshot  int64       `json:"snapshot"`
	CreatedAt time.Time   `json:"created_at"`
	Folder    string      `json:"folder,omitempty"`
	Folders   []LsFolder  `json:"folders,omitempty"`
	Messages  []LsMessage `json:"messages,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
}

// Ls lists the folders of a snapshot, or with folder set the messages in it. It
// opens sealed names and metadata, so an encrypted repository needs the private key.
func Ls(ctx context.Context, cfg *config.Config, snapshot, folder string) (*LsResult, error) {
	rd, err := openReader(ctx, cfg, true)
	if err != nil {
		return nil, err
	}
	defer rd.close()
	id, err := rd.snapshotID(snapshot)
	if err != nil {
		return nil, err
	}
	snaps, err := rd.db.Snapshots()
	if err != nil {
		return nil, err
	}
	res := &LsResult{Snapshot: id, Warnings: rd.warnings}
	for _, s := range snaps {
		if s.ID == id {
			res.CreatedAt = s.CreatedAt
		}
	}
	folders, err := rd.db.SnapshotFolders(id)
	if err != nil {
		return nil, err
	}

	if folder == "" {
		counts, err := rd.db.SnapshotFolderCounts(id)
		if err != nil {
			return nil, err
		}
		for _, f := range folders {
			name, err := rd.opener.Open(f.NameSealed)
			if err != nil {
				return nil, fmt.Errorf("folder %d: %w", f.ID, err)
			}
			res.Folders = append(res.Folders, LsFolder{Name: string(name), SpecialUse: f.SpecialUse, Messages: counts[f.ID]})
		}
		sortFolders(res.Folders)
		return res, nil
	}

	key, err := rd.r.FolderKey()
	if err != nil {
		return nil, err
	}
	want := seal.FolderHash(key, folder)
	var found *index.Folder
	for i := range folders {
		if folders[i].NameHash == want {
			found = &folders[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("snapshot %d has no folder %q", id, folder)
	}
	res.Folder = folder
	entries, err := rd.db.SnapshotEntries(id, found.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := openMeta(rd.opener, e.MetaSealed)
		if err != nil {
			return nil, fmt.Errorf("message %x: %w", e.Blob[:8], err)
		}
		res.Messages = append(res.Messages, LsMessage{
			UID: e.UID, UIDValidity: e.UIDValidity, Flags: e.Flags, Size: e.Size,
			SHA256: fmt.Sprintf("%x", e.Blob), Date: m.InternalDate, From: m.From, Subject: m.Subject, MessageID: m.MessageID,
		})
	}
	return res, nil
}

func sortFolders(fs []LsFolder) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].Name < fs[j].Name })
}

// ShowResult is one message.
type ShowResult struct {
	SHA256   string      `json:"sha256"`
	Size     int64       `json:"size"`
	Meta     MessageMeta `json:"meta"`
	Raw      []byte      `json:"-"` // only with raw=true
	Warnings []string    `json:"warnings,omitempty"`
}

// Show finds a message by a hex prefix of its SHA-256 (at least 8 characters).
// An ambiguous prefix is an error. With raw it also reads and returns the exact
// bytes from the object store, hash-verified. It needs the private key when the
// repository is encrypted.
func Show(ctx context.Context, cfg *config.Config, prefix string, raw bool) (*ShowResult, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if len(prefix) < minHashPrefix {
		return nil, fmt.Errorf("a hash prefix needs at least %d hex characters (got %d)", minHashPrefix, len(prefix))
	}
	rd, err := openReader(ctx, cfg, true)
	if err != nil {
		return nil, err
	}
	defer rd.close()
	rows, err := rd.db.BlobsByPrefix(prefix, 2)
	if err != nil {
		return nil, err
	}
	switch len(rows) {
	case 0:
		return nil, fmt.Errorf("no message has a hash starting with %s", prefix)
	case 2:
		return nil, fmt.Errorf("the prefix %s matches more than one message; give more characters", prefix)
	}
	b := rows[0]
	m, err := openMeta(rd.opener, b.MetaSealed)
	if err != nil {
		return nil, fmt.Errorf("message %x: %w", b.SHA[:8], err)
	}
	res := &ShowResult{SHA256: fmt.Sprintf("%x", b.SHA), Size: b.Size, Meta: m, Warnings: rd.warnings}
	if raw {
		st, err := openStore(rd.r, rd.identity)
		if err != nil {
			return nil, err
		}
		defer func() { _ = st.Close() }()
		if res.Raw, err = st.Get(objects.Key(b.SHA)); err != nil {
			return nil, fmt.Errorf("message %x: %w", b.SHA[:8], err)
		}
	}
	return res, nil
}
