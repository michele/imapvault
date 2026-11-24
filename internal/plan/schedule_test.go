package plan

import (
	"testing"
	"time"
)

var sched0 = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func sched() Schedule {
	return Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, StaleAfter: 24 * time.Hour}
}

// afterBackup is a state where one backup started at sched0, took d and succeeded.
func afterBackup(d time.Duration) RunState {
	return RunState{
		Started: sched0, LastBackupStart: sched0, LastBackupEnd: sched0.Add(d), LastBackupOK: sched0.Add(d),
	}
}

func TestNextJob(t *testing.T) {
	min := time.Minute
	cases := []struct {
		name string
		s    Schedule
		st   RunState
		now  time.Time
		job  Job
		wait time.Duration
	}{
		{"a fresh process backs up at once", sched(), RunState{Started: sched0}, sched0, JobBackup, 0},

		{"forget follows the first successful backup", sched(), afterBackup(10 * time.Second), sched0.Add(10 * time.Second), JobForget, 0},
		{"then the next backup is an interval after the previous START", sched(),
			withForget(afterBackup(10*time.Second), sched0.Add(10*time.Second)), sched0.Add(10 * time.Second), JobBackup, time.Hour - 10*time.Second},
		{"a run longer than the interval is followed at once (it resumes)", sched(),
			withForget(afterBackup(3*time.Hour), sched0.Add(3*time.Hour)), sched0.Add(3 * time.Hour), JobBackup, 0},

		{"first failure retries in 30s", sched(), failed(1, 5*time.Second), sched0.Add(5 * time.Second), JobBackup, 30 * time.Second},
		{"second in 1m", sched(), failed(2, 5*time.Second), sched0.Add(5 * time.Second), JobBackup, min},
		{"third in 2m", sched(), failed(3, 5*time.Second), sched0.Add(5 * time.Second), JobBackup, 2 * min},
		{"later ones in 5m", sched(), failed(9, 5*time.Second), sched0.Add(5 * time.Second), JobBackup, 5 * min},
		{"never later than the interval", Schedule{Every: 2 * min}, failed(9, 5*time.Second), sched0.Add(5 * time.Second), JobBackup, 2 * min},
		{"waiting is counted from the end of the failed run", sched(), failed(1, 5*time.Second), sched0.Add(20 * time.Second), JobBackup, 15 * time.Second},

		{"a locked repository is retried in 1m", sched(), RunState{Started: sched0, LastBackupStart: sched0, LastBackupEnd: sched0.Add(time.Second), Busy: true}, sched0.Add(time.Second), JobBackup, min},

		{"forget off", Schedule{Every: time.Hour}, afterBackup(time.Second), sched0.Add(time.Second), JobBackup, time.Hour - time.Second},
		{"forget waits for a successful backup in this process", sched(), failed(1, time.Second), sched0.Add(time.Second), JobBackup, 30 * time.Second},
		{"forget is a day after the last forget", sched(),
			RunState{Started: sched0, LastBackupStart: sched0.Add(23 * time.Hour), LastBackupEnd: sched0.Add(23 * time.Hour), LastBackupOK: sched0.Add(23 * time.Hour), LastForget: sched0.Add(time.Minute)},
			sched0.Add(23*time.Hour + time.Second), JobBackup, time.Hour - time.Second},
		{"forget comes due while waiting for the backup", sched(),
			RunState{Started: sched0, LastBackupStart: sched0.Add(23*time.Hour + 30*time.Minute), LastBackupEnd: sched0.Add(23*time.Hour + 30*time.Minute), LastBackupOK: sched0.Add(23*time.Hour + 30*time.Minute), LastForget: sched0.Add(time.Minute)},
			sched0.Add(24*time.Hour + time.Minute), JobForget, 0},

		{"prune is off by default", sched(), withForget(afterBackup(time.Second), sched0.Add(time.Second)), sched0.Add(30 * 24 * time.Hour), JobBackup, 0},
		{"prune waits a full period after start", Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, PruneEvery: 7 * 24 * time.Hour},
			withForget(afterBackup(time.Second), sched0.Add(time.Second)), sched0.Add(time.Second), JobBackup, time.Hour - time.Second},
		{"prune comes due", Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, PruneEvery: 7 * 24 * time.Hour},
			RunState{Started: sched0, LastBackupStart: sched0.Add(7*24*time.Hour - time.Minute), LastBackupEnd: sched0.Add(7*24*time.Hour - time.Minute), LastBackupOK: sched0.Add(7*24*time.Hour - time.Minute), LastForget: sched0.Add(7*24*time.Hour - 30*time.Second)},
			sched0.Add(7 * 24 * time.Hour), JobPrune, 0},
		{"a backup that is due goes before forget and prune", Schedule{Every: time.Hour, ForgetEvery: time.Hour, PruneEvery: time.Hour},
			RunState{Started: sched0, LastBackupStart: sched0, LastBackupEnd: sched0, LastBackupOK: sched0, LastForget: sched0}, sched0.Add(2 * time.Hour), JobBackup, 0},
		{"forget goes before prune", Schedule{Every: time.Hour, ForgetEvery: time.Hour, PruneEvery: time.Hour},
			RunState{Started: sched0, LastBackupStart: sched0.Add(2 * time.Hour), LastBackupEnd: sched0.Add(2 * time.Hour), LastBackupOK: sched0.Add(2 * time.Hour), LastForget: sched0},
			sched0.Add(2 * time.Hour), JobForget, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job, wait := NextJob(c.s, c.st, c.now)
			if job != c.job || wait != c.wait {
				t.Fatalf("got %v in %v, want %v in %v", job, wait, c.job, c.wait)
			}
		})
	}
}

func withForget(st RunState, at time.Time) RunState { st.LastForget = at; return st }

func failed(n int, took time.Duration) RunState {
	return RunState{Started: sched0, LastBackupStart: sched0, LastBackupEnd: sched0.Add(took), Failures: n}
}

func TestStale(t *testing.T) {
	s := sched()
	cases := []struct {
		name string
		s    Schedule
		st   RunState
		now  time.Time
		want bool
	}{
		{"healthy", s, afterBackup(time.Second), sched0.Add(48 * time.Hour), false},
		{"failing for less than the limit", s, RunState{Started: sched0, LastBackupOK: sched0, Failures: 3}, sched0.Add(23 * time.Hour), false},
		{"failing for the limit", s, RunState{Started: sched0, LastBackupOK: sched0, Failures: 3}, sched0.Add(24 * time.Hour), true},
		{"never succeeded: counted from the start", s, RunState{Started: sched0, Failures: 5}, sched0.Add(25 * time.Hour), true},
		{"told once per streak", s, RunState{Started: sched0, LastBackupOK: sched0, Failures: 3, StaleNotified: true}, sched0.Add(30 * time.Hour), false},
		{"a locked repository is not a failure", s, RunState{Started: sched0, LastBackupOK: sched0, Busy: true}, sched0.Add(30 * time.Hour), false},
		{"off", Schedule{Every: time.Hour}, RunState{Started: sched0, LastBackupOK: sched0, Failures: 3}, sched0.Add(300 * time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Stale(c.s, c.st, c.now); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 0, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 5 * time.Minute, 50: 5 * time.Minute} {
		if got := Backoff(n); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestValidateSchedule(t *testing.T) {
	min := 5 * time.Minute
	cases := []struct {
		name string
		s    Schedule
		ok   bool
	}{
		{"defaults", Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, StaleAfter: 24 * time.Hour}, true},
		{"the floor itself", Schedule{Every: min}, true},
		{"too often", Schedule{Every: time.Minute}, false},
		{"zero interval", Schedule{}, false},
		{"negative", Schedule{Every: time.Hour, ForgetEvery: -time.Hour}, false},
		{"prune needs forget", Schedule{Every: time.Hour, PruneEvery: 7 * 24 * time.Hour}, false},
		{"prune with forget", Schedule{Every: time.Hour, ForgetEvery: 24 * time.Hour, PruneEvery: 7 * 24 * time.Hour}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Validate(min)
			if (err == nil) != c.ok {
				t.Fatalf("got %v, want ok=%v", err, c.ok)
			}
		})
	}
}
