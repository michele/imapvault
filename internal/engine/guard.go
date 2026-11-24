package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/plan"
)

// maxSummaryFolders caps how many folders an alert summary names.
const maxSummaryFolders = 10

// AlertReport is the guard alert a run raised.
type AlertReport struct {
	ID             int64  `json:"id"`
	Kind           string `json:"kind"`
	Summary        string `json:"summary"`
	PinnedSnapshot int64  `json:"pinned_snapshot,omitempty"`
	HookError      string `json:"hook_error,omitempty"`
}

// guard compares this run's result with the previous snapshot. It reads only
// plaintext (positions, blob keys, counts), so it works without the private key.
// It returns nil when nothing is wrong or there is no previous snapshot.
//
// Comparing with the previous *snapshot* (not the previous run) means deletions
// that a failed run committed and the next run then sees are still counted, and
// after an alert the next run compares with the flagged snapshot, so it does not
// alert again for the same loss.
func (b *backup) guard() (*index.NewAlert, string, error) {
	snaps, err := b.db.Snapshots()
	if err != nil || len(snaps) == 0 {
		return nil, "", err
	}
	prev := snaps[len(snaps)-1]
	removed, added, err := b.db.SnapshotChanges(prev.ID, b.run)
	if err != nil {
		return nil, "", err
	}
	conv := func(cs []index.SnapChange) []plan.DiffEntry {
		out := make([]plan.DiffEntry, len(cs))
		for i, c := range cs {
			out[i] = plan.DiffEntry{FolderID: c.FolderID, UIDValidity: c.UIDValidity, UID: c.UID, Blob: c.Blob, Flags: c.Flags}
		}
		return out
	}
	per := plan.DiffSnapshots(conv(removed), conv(added))

	folders, err := b.db.FoldersInEither(prev.ID, b.run)
	if err != nil {
		return nil, "", err
	}
	var vanished []int64
	for _, f := range folders {
		if f.GoneRun > prev.ID && f.GoneRun <= b.run {
			vanished = append(vanished, f.ID)
		}
	}
	res := plan.CheckGuard(plan.GuardInput{
		PrevSnapshot: prev.ID, PrevMessages: prev.Messages, Folders: per, Vanished: vanished,
	}, plan.GuardPolicy{MaxDeletedPercent: b.cfg.Guard.MaxDeletedPercent, MaxDeletedCount: b.cfg.Guard.MaxDeletedCount})
	if !res.Tripped {
		return nil, "", nil
	}
	summary := b.summarize(res, per, prev)
	sealed, err := b.sealer.Seal([]byte(summary))
	if err != nil {
		return nil, "", err
	}
	return &index.NewAlert{Kind: res.Kind, SummarySealed: sealed, PinSnapshot: prev.ID}, summary, nil
}

// folderLabel names a folder for a summary. Folders the run read have names; a
// folder that vanished does not (its name is sealed and backup holds no private
// key), so it shows as #id, which `diff` and `ls` map back with the key.
func (b *backup) folderLabel(id int64) string {
	if n, ok := b.folderNames[id]; ok {
		return cleanText(n)
	}
	return fmt.Sprintf("#%d", id)
}

// summarize describes a tripped guard in one line: counts and folder names only
// (rule 6: never subjects or bodies).
func (b *backup) summarize(res plan.GuardResult, per map[int64]plan.FolderDiff, prev index.Snapshot) string {
	var sb strings.Builder
	if prev.Messages > 0 {
		fmt.Fprintf(&sb, "%d of %d messages deleted (%.1f%%) since snapshot %d", res.Deleted, prev.Messages, res.Percent, prev.ID)
	} else {
		fmt.Fprintf(&sb, "%d messages deleted since snapshot %d", res.Deleted, prev.ID)
	}
	var parts []string
	for i, f := range res.ByFolder {
		if i == maxSummaryFolders {
			parts = append(parts, fmt.Sprintf("and %d more folders", len(res.ByFolder)-maxSummaryFolders))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", b.folderLabel(f.FolderID), f.Deleted))
	}
	if len(parts) > 0 {
		sb.WriteString(": " + strings.Join(parts, ", "))
	}
	for _, id := range res.VanishedWithLoss {
		fmt.Fprintf(&sb, "; folder %s vanished (%d messages deleted)", b.folderLabel(id), per[id].Deleted)
	}
	return sb.String()
}

// notify sends the alert through guard.on_alert and guard.on_alert_url. A failing
// notification never fails the run: the alert is already recorded, so it is
// reported as a warning and in the alert's HookError.
func (b *backup) notify(ctx context.Context, ar *AlertReport) {
	// Ctrl-C cancelled the run's context, but the alert still has to go out.
	cmdErr, urlErr := Notify(context.WithoutCancel(ctx), b.cfg, Notification{
		Event: EventGuard, Summary: ar.Summary, AlertID: ar.ID, Snapshot: b.rep.Snapshot,
	})
	var failed []string
	if cmdErr != nil {
		b.warn("guard.on_alert failed: %v", cmdErr)
		failed = append(failed, cmdErr.Error())
	}
	if urlErr != nil {
		b.warn("%v", urlErr)
		failed = append(failed, urlErr.Error())
	}
	ar.HookError = strings.Join(failed, "; ")
}
