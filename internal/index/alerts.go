package index

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NewAlert is an alert written together with the snapshot of the run that
// raised it. The summary is sealed by the caller: the index never sees plaintext.
type NewAlert struct {
	Kind          string
	SummarySealed []byte
	// PinSnapshot is the last good snapshot, which the alert pins. 0 pins nothing.
	PinSnapshot int64
}

// Alert is a row of alerts.
type Alert struct {
	ID             int64
	RunID          int64 // the run (and snapshot) that tripped the guard
	Kind           string
	SummarySealed  []byte
	CreatedAt      time.Time
	AcknowledgedAt time.Time // zero while unacknowledged
	// PinnedSnapshot is the snapshot the alert pinned: the newest one before RunID.
	PinnedSnapshot int64
}

// Acknowledged reports whether the alert was acknowledged.
func (a Alert) Acknowledged() bool { return !a.AcknowledgedAt.IsZero() }

func recordAlert(tx *sql.Tx, run int64, now time.Time, a NewAlert) error {
	if _, err := tx.Exec(`UPDATE snapshots SET flagged = 1 WHERE id = ?`, run); err != nil {
		return err
	}
	if a.PinSnapshot != 0 {
		res, err := tx.Exec(`UPDATE snapshots SET pinned = 1 WHERE id = ?`, a.PinSnapshot)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("pin: snapshot %d does not exist", a.PinSnapshot)
		}
	}
	_, err := tx.Exec(`INSERT INTO alerts(run_id, kind, summary_sealed, created_at) VALUES (?, ?, ?, ?)`,
		run, a.Kind, a.SummarySealed, ts(now))
	return err
}

// LastAlertID returns the id of the alert raised by run, or 0.
func (db *DB) LastAlertID(run int64) (int64, error) {
	var id sql.NullInt64
	if err := db.QueryRow(`SELECT max(id) FROM alerts WHERE run_id = ?`, run).Scan(&id); err != nil {
		return 0, err
	}
	return id.Int64, nil
}

const alertCols = `a.id, a.run_id, a.kind, a.summary_sealed, a.created_at, coalesce(a.acknowledged_at, ''),
	coalesce((SELECT max(s.id) FROM snapshots s WHERE s.id < a.run_id), 0)`

// Alerts lists alerts, oldest first. Without all only unacknowledged ones.
func (db *DB) Alerts(all bool) ([]Alert, error) {
	q := `SELECT ` + alertCols + ` FROM alerts a`
	if !all {
		q += ` WHERE a.acknowledged_at IS NULL`
	}
	rows, err := db.Query(q + ` ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		var a Alert
		var created, acked string
		if err := rows.Scan(&a.ID, &a.RunID, &a.Kind, &a.SummarySealed, &created, &acked, &a.PinnedSnapshot); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTS(created)
		if acked != "" {
			a.AcknowledgedAt = parseTS(acked)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UnackedAlertCount is the number of alerts nobody acknowledged yet. prune
// refuses to run while it is not zero.
func (db *DB) UnackedAlertCount() (int, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM alerts WHERE acknowledged_at IS NULL`).Scan(&n)
	return n, err
}

// AckAlerts acknowledges alerts in one transaction. An unknown id is an error and
// acknowledges nothing; acknowledging one twice keeps the first time.
func (db *DB) AckAlerts(ids []int64, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM alerts WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("there is no alert %d", id)
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE alerts SET acknowledged_at = ? WHERE id = ? AND acknowledged_at IS NULL`, ts(now), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetPinned pins or unpins a snapshot.
func (db *DB) SetPinned(id int64, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	res, err := db.Exec(`UPDATE snapshots SET pinned = ? WHERE id = ?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("there is no snapshot %d", id)
	}
	return nil
}

// PinnedByUnackedAlert returns the id of an unacknowledged alert that pinned
// snapshot id (the newest snapshot before the alert's run), if there is one.
func (db *DB) PinnedByUnackedAlert(id int64) (int64, bool, error) {
	var alert int64
	err := db.QueryRow(`SELECT a.id FROM alerts a
		WHERE a.acknowledged_at IS NULL
		  AND (SELECT max(s.id) FROM snapshots s WHERE s.id < a.run_id) = ?
		ORDER BY a.id LIMIT 1`, id).Scan(&alert)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return alert, err == nil, err
}
