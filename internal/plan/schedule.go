package plan

import (
	"errors"
	"fmt"
	"time"
)

// Job is one thing `imapvault run` does.
type Job int

// The jobs, in the order they win when several are due at once.
const (
	JobBackup Job = iota
	JobForget
	JobPrune
)

func (j Job) String() string {
	switch j {
	case JobBackup:
		return "backup"
	case JobForget:
		return "forget"
	case JobPrune:
		return "prune"
	}
	return fmt.Sprintf("job(%d)", int(j))
}

// Schedule is how often `run` does each job. A zero ForgetEvery, PruneEvery or
// StaleAfter switches that job off.
type Schedule struct {
	Every       time.Duration // between backup starts
	ForgetEvery time.Duration
	PruneEvery  time.Duration
	StaleAfter  time.Duration // warn when no backup has succeeded for this long
}

// RunState is what `run` remembers about itself. It lives in memory only: a
// restart starts again from a fresh state, which is safe because every job is
// idempotent.
type RunState struct {
	Started         time.Time // when this process began
	LastBackupStart time.Time // zero until a backup has been attempted
	LastBackupEnd   time.Time
	LastBackupOK    time.Time // zero until a backup has succeeded in this process
	Failures        int       // consecutive failed backups; a locked repository does not count
	Busy            bool      // the last attempt found the repository locked by someone else
	LastForget      time.Time // last attempt, whatever its outcome, so a failing forget cannot spin
	LastPrune       time.Time
	StaleNotified   bool // the stale warning went out for the current failure streak
}

const busyRetry = time.Minute

// Backoff is how long to wait before retrying a backup that has failed n times
// in a row: 30 s, 1 m, 2 m, then 5 m.
func Backoff(n int) time.Duration {
	switch {
	case n <= 0:
		return 0
	case n == 1:
		return 30 * time.Second
	case n == 2:
		return time.Minute
	case n == 3:
		return 2 * time.Minute
	}
	return 5 * time.Minute
}

// Validate refuses a schedule that makes no sense. minEvery is the shortest
// backup interval allowed: every run writes a copy of the index, and the
// mass-deletion guard compares each run with the previous one, so very short
// intervals are not free.
func (s Schedule) Validate(minEvery time.Duration) error {
	switch {
	case s.Every < minEvery:
		return fmt.Errorf("--every %s is too short: the minimum is %s", s.Every, minEvery)
	case s.ForgetEvery < 0, s.PruneEvery < 0, s.StaleAfter < 0:
		return errors.New("intervals cannot be negative")
	case s.PruneEvery > 0 && s.ForgetEvery == 0:
		return errors.New("--prune-every needs --forget-every: prune only removes what forget has released")
	}
	return nil
}

// NextJob says which job to run next and how long to wait before starting it
// (zero: now). When several are due, backup goes before forget before prune.
// Pure: the caller supplies the clock.
func NextJob(s Schedule, st RunState, now time.Time) (Job, time.Duration) {
	type candidate struct {
		job Job
		at  time.Time
	}
	cands := []candidate{{JobBackup, backupAt(s, st)}}
	if s.ForgetEvery > 0 && !st.LastBackupOK.IsZero() {
		at := st.LastBackupOK // never forgotten in this process: due right after the first good backup
		if !st.LastForget.IsZero() {
			at = st.LastForget.Add(s.ForgetEvery)
		}
		cands = append(cands, candidate{JobForget, at})
	}
	if s.PruneEvery > 0 {
		base := st.Started
		if !st.LastPrune.IsZero() {
			base = st.LastPrune
		}
		cands = append(cands, candidate{JobPrune, base.Add(s.PruneEvery)})
	}
	// A due job wins by priority; otherwise the soonest one, ties by priority.
	best := cands[0]
	for _, c := range cands[1:] {
		bestDue, cDue := !best.at.After(now), !c.at.After(now)
		switch {
		case bestDue:
			// the earlier candidate in the list has priority
		case cDue || c.at.Before(best.at):
			best = c
		}
	}
	if wait := best.at.Sub(now); wait > 0 {
		return best.job, wait
	}
	return best.job, 0
}

func backupAt(s Schedule, st RunState) time.Time {
	switch {
	case st.LastBackupStart.IsZero():
		return st.Started
	case st.Busy:
		return st.LastBackupEnd.Add(busyRetry)
	case st.Failures > 0:
		return st.LastBackupEnd.Add(min(Backoff(st.Failures), s.Every))
	}
	return st.LastBackupStart.Add(s.Every)
}

// Stale reports whether the "no successful backup for a while" warning is due:
// backups have been failing for StaleAfter (counted from the last success, or
// from the start if there was none) and this streak has not been announced yet.
func Stale(s Schedule, st RunState, now time.Time) bool {
	if s.StaleAfter <= 0 || st.Failures == 0 || st.Busy || st.StaleNotified {
		return false
	}
	since := st.LastBackupOK
	if since.IsZero() {
		since = st.Started
	}
	return now.Sub(since) >= s.StaleAfter
}
