package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/michele/imapvault/internal/config"
	"github.com/michele/imapvault/internal/index"
	"github.com/michele/imapvault/internal/repo"
	"github.com/michele/imapvault/internal/seal"
)

// AlertInfo is one line of `alerts`.
type AlertInfo struct {
	ID             int64      `json:"id"`
	Snapshot       int64      `json:"snapshot"`                  // the flagged snapshot the run wrote
	PinnedSnapshot int64      `json:"pinned_snapshot,omitempty"` // the last good snapshot the alert pinned
	Kind           string     `json:"kind"`
	CreatedAt      time.Time  `json:"created_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	Summary        string     `json:"summary,omitempty"` // needs the private key on an encrypted repository
}

// AlertsResult is the alerts of a repository.
type AlertsResult struct {
	Alerts   []AlertInfo `json:"alerts"`
	Warnings []string    `json:"warnings,omitempty"`
}

// Alerts lists guard alerts, oldest first; without all only the unacknowledged
// ones. Summaries are sealed, so they are shown when the private key is available
// (or the repository is plain) and left out otherwise: everything else here is
// plaintext, so the command works on a backup machine.
func Alerts(ctx context.Context, cfg *config.Config, all bool) (*AlertsResult, error) {
	rd, err := openReaderMode(ctx, cfg, keyIfAvailable)
	if err != nil {
		return nil, err
	}
	defer rd.close()
	rows, err := rd.db.Alerts(all)
	if err != nil {
		return nil, err
	}
	res := &AlertsResult{Alerts: []AlertInfo{}, Warnings: rd.warnings}
	for _, a := range rows {
		info := AlertInfo{ID: a.ID, Snapshot: a.RunID, PinnedSnapshot: a.PinnedSnapshot, Kind: a.Kind, CreatedAt: a.CreatedAt}
		if a.Acknowledged() {
			t := a.AcknowledgedAt
			info.AcknowledgedAt = &t
		}
		if rd.names {
			s, err := rd.opener.Open(a.SummarySealed)
			if err != nil {
				return nil, fmt.Errorf("alert %d: %w", a.ID, err)
			}
			info.Summary = string(s)
		}
		res.Alerts = append(res.Alerts, info)
	}
	return res, nil
}

// openWriter opens the repository for a small index write (ack, pin). It needs no
// key: it touches only plaintext columns. The exclusive lock is non-blocking, so
// it fails at once while a backup runs.
func openWriter(cfg *config.Config) (*reader, error) {
	acct := accountOf(cfg)
	r, err := repo.Open(cfg.Repository.Path, &acct)
	if err != nil {
		return nil, err
	}
	unlock, err := r.Lock(repo.Exclusive)
	if err != nil {
		return nil, err
	}
	db, err := index.Open(r.IndexPath(), index.Options{})
	if err != nil {
		unlock()
		return nil, err
	}
	return &reader{r: r, db: db, opener: seal.NewOpener(nil), unlock: unlock}, nil
}

// Ack acknowledges alerts: the listed ids, or every unacknowledged one with all.
// It returns the ids it acknowledged. Acknowledging an alert already
// acknowledged is not an error.
func Ack(_ context.Context, cfg *config.Config, ids []int64, all bool) ([]int64, error) {
	if all == (len(ids) > 0) {
		return nil, errors.New("give alert ids to acknowledge, or --all")
	}
	w, err := openWriter(cfg)
	if err != nil {
		return nil, err
	}
	defer w.close()
	if all {
		open, err := w.db.Alerts(false)
		if err != nil {
			return nil, err
		}
		for _, a := range open {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err := w.db.AckAlerts(ids, time.Now()); err != nil {
		return nil, err
	}
	return ids, nil
}

// PinResult is what pin and unpin did.
type PinResult struct {
	Snapshot int64 `json:"snapshot"`
	Pinned   bool  `json:"pinned"`
}

// Pin pins (or unpins) a snapshot, which retention then always keeps. spec is a
// snapshot number, "latest" or "latest~N". A snapshot that an unacknowledged
// guard alert pinned cannot be unpinned until the alert is acknowledged.
func Pin(_ context.Context, cfg *config.Config, spec string, pinned bool) (*PinResult, error) {
	w, err := openWriter(cfg)
	if err != nil {
		return nil, err
	}
	defer w.close()
	id, err := w.snapshotID(spec)
	if err != nil {
		return nil, err
	}
	if !pinned {
		if alert, ok, err := w.db.PinnedByUnackedAlert(id); err != nil {
			return nil, err
		} else if ok {
			return nil, fmt.Errorf("snapshot %d was pinned by guard alert %d, which is not acknowledged yet: run `imapvault ack %d` first", id, alert, alert)
		}
	}
	if err := w.db.SetPinned(id, pinned); err != nil {
		return nil, err
	}
	return &PinResult{Snapshot: id, Pinned: pinned}, nil
}
