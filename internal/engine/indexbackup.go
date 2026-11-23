package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/repo"
)

// keepIndexBackups is how many VACUUM INTO copies of the index are kept.
const keepIndexBackups = 7

var indexBackupName = regexp.MustCompile(`^index-\d{8}T\d{6}Z-[A-Za-z0-9]+\.db$`)

// writeIndexBackup writes a consistent copy of the index to index-backups/ and
// drops the oldest copies beyond the newest keepIndexBackups. label says what
// caused it ("r12" for the backup run, "prune"). The copy is built in tmp/ and
// renamed into place, so a crash leaves a tmp file (which prune removes) and
// never a half-written backup. Sealed columns are copied as they are: it needs
// no key. The caller holds the exclusive repository lock.
func writeIndexBackup(r *repo.Repo, db *index.DB, label string, now time.Time) (string, error) {
	name := fmt.Sprintf("index-%s-%s.db", now.UTC().Format("20060102T150405Z"), label)
	tmp := filepath.Join(r.TmpDir(), name+".tmp")
	final := filepath.Join(r.IndexBackupsDir(), name)
	_ = os.Remove(tmp)
	if err := db.VacuumInto(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy the index: %w", err)
	}
	fail := func(err error) (string, error) {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fail(err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail(err)
	}
	if err := syncDir(r.IndexBackupsDir()); err != nil {
		return final, err
	}
	return final, rotateIndexBackups(r.IndexBackupsDir())
}

// rotateIndexBackups removes all but the newest keepIndexBackups copies. Names
// start with their UTC timestamp, so name order is age order. Files that do not
// look like our copies are left alone.
func rotateIndexBackups(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && indexBackupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepIndexBackups {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
