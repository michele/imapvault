package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/objects"
	"github.com/michele/imapvault/internal/repo"
)

// maxFindingsPerKind caps how many findings of one kind a report lists; the rest
// are summarised in one extra finding so a ruined repository stays readable.
const maxFindingsPerKind = 20

// Finding severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Finding is one thing check noticed. Messages carry hashes, counts and file
// names, never message content.
type Finding struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
}

// CheckOptions tune Check.
type CheckOptions struct {
	// ReadData re-reads and re-hashes every object file. Without it check only
	// proves that every indexed blob has an object file.
	ReadData bool
}

// CheckReport is the result of Check. Errors mean the repository cannot be
// trusted (missing or corrupt data, a damaged index). Warnings are leftovers a
// crash may legitimately produce (see the durability rule) and prune cleans up.
type CheckReport struct {
	Repository      string    `json:"repository"`
	FormatVersion   int       `json:"format_version"`
	SchemaVersion   int       `json:"schema_version"`
	Blobs           int       `json:"blobs"`            // rows in the index
	Objects         int       `json:"objects"`          // object files on disk
	ObjectsVerified int       `json:"objects_verified"` // read and re-hashed (--read-data)
	Errors          []Finding `json:"errors"`
	Warnings        []Finding `json:"warnings"`

	perKind map[string]int
}

// OK reports whether check found no errors (warnings do not count).
func (r *CheckReport) OK() bool { return len(r.Errors) == 0 }

func (r *CheckReport) add(severity, kind, format string, args ...any) {
	if r.perKind == nil {
		r.perKind = map[string]int{}
	}
	key := severity + "/" + kind
	r.perKind[key]++
	if r.perKind[key] > maxFindingsPerKind {
		return // counted; summarised by finish
	}
	f := Finding{Severity: severity, Kind: kind, Message: fmt.Sprintf(format, args...)}
	if severity == SeverityError {
		r.Errors = append(r.Errors, f)
	} else {
		r.Warnings = append(r.Warnings, f)
	}
}

// finish appends a "+N more" finding for every kind that hit the cap.
func (r *CheckReport) finish() {
	for _, sev := range []string{SeverityError, SeverityWarning} {
		for _, kind := range sortedKinds(r.perKind, sev) {
			if extra := r.perKind[sev+"/"+kind] - maxFindingsPerKind; extra > 0 {
				f := Finding{Severity: sev, Kind: kind, Message: fmt.Sprintf("%d more %s findings not listed", extra, kind)}
				if sev == SeverityError {
					r.Errors = append(r.Errors, f)
				} else {
					r.Warnings = append(r.Warnings, f)
				}
			}
		}
	}
}

// Check verifies a repository without changing it. It holds a shared lock, so it
// is refused (repo.ErrLocked) while a backup or prune is running. A repository
// that cannot be opened at all (not a repository, wrong account, other format
// version) is returned as an error; problems inside an open repository are
// findings in the report.
func Check(ctx context.Context, cfg *config.Config, opts CheckOptions) (*CheckReport, error) {
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		return nil, err
	}
	unlock, err := r.Lock(repo.Shared)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rep := &CheckReport{Repository: r.Path, FormatVersion: r.Meta.FormatVersion}

	haveDirs := true
	for _, d := range []string{r.ObjectsDir(), r.TmpDir(), r.IndexBackupsDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			rep.add(SeverityError, "missing-dir", "directory %s is missing", filepath.Base(d))
			haveDirs = false
		}
	}

	indexed, haveIndex := checkIndex(ctx, r, rep)
	if haveDirs {
		if err := checkObjects(ctx, r, rep, indexed, haveIndex, opts); err != nil {
			return nil, err
		}
		checkTmp(r, rep)
	}
	rep.finish()
	return rep, nil
}

// checkIndex opens the index read-only, runs its own consistency checks and
// returns the set of blob keys it references. ok is false when the index could
// not be read, in which case orphan detection is skipped.
func checkIndex(ctx context.Context, r *repo.Repo, rep *CheckReport) (keys map[objects.Key]struct{}, ok bool) {
	db, err := index.Open(r.IndexPath(), index.Options{ReadOnly: true})
	if err != nil {
		rep.add(SeverityError, "index-open", "cannot open the index: %v", err)
		return nil, false
	}
	defer func() { _ = db.Close() }()
	rep.SchemaVersion, _ = db.SchemaVersion()

	problems, err := db.IntegrityCheck()
	if err != nil {
		rep.add(SeverityError, "index-corrupt", "integrity check failed to run: %v", err)
		return nil, false
	}
	for _, p := range problems {
		rep.add(SeverityError, "index-corrupt", "sqlite integrity_check: %s", p)
	}
	if len(problems) > 0 {
		return nil, false // do not trust anything we would read from a damaged index
	}

	violations, err := db.ForeignKeyCheck()
	if err != nil {
		rep.add(SeverityError, "index-corrupt", "foreign key check failed to run: %v", err)
		return nil, false
	}
	for _, v := range violations {
		rep.add(SeverityError, "dangling-reference", "%s row %d references a missing %s row", v.Table, v.RowID, v.Parent)
	}

	keys = map[objects.Key]struct{}{}
	err = db.BlobKeys(func(k [32]byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		keys[objects.Key(k)] = struct{}{}
		return nil
	})
	if err != nil {
		rep.add(SeverityError, "index-corrupt", "reading blobs: %v", err)
		return nil, false
	}
	rep.Blobs = len(keys)
	return keys, true
}

func checkObjects(ctx context.Context, r *repo.Repo, rep *CheckReport, indexed map[objects.Key]struct{}, haveIndex bool, opts CheckOptions) error {
	st, err := objects.Open(r.ObjectsDir(), r.TmpDir())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	onDisk := map[objects.Key]struct{}{}
	err = st.Walk(func(k objects.Key) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		onDisk[k] = struct{}{}
		if haveIndex {
			if _, ok := indexed[k]; !ok {
				rep.add(SeverityWarning, "orphan-object", "object %s is not referenced by the index (prune removes it)", k)
			}
		}
		if opts.ReadData {
			rep.ObjectsVerified++
			if err := st.Verify(k); err != nil {
				rep.add(SeverityError, "corrupt-object", "%v", err)
			}
		}
		return nil
	}, func(path string) {
		rep.add(SeverityWarning, "unexpected-file", "unexpected entry %s in objects/", relTo(r.Path, path))
	})
	if err != nil {
		return err
	}
	rep.Objects = len(onDisk)

	for k := range indexed {
		if _, ok := onDisk[k]; !ok {
			rep.add(SeverityError, "missing-object", "object %s is referenced by the index but missing on disk", k)
		}
	}
	return nil
}

func checkTmp(r *repo.Repo, rep *CheckReport) {
	ents, err := os.ReadDir(r.TmpDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		rep.add(SeverityWarning, "tmp-leftover", "tmp/%s is left over from an interrupted write (prune removes it)", e.Name())
	}
}

func relTo(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil {
		return rel
	}
	return path
}

func sortedKinds(perKind map[string]int, severity string) []string {
	var out []string
	for k := range perKind {
		if kind, ok := strings.CutPrefix(k, severity+"/"); ok {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}
