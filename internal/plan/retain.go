package plan

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Reasons a snapshot is kept, in the order they are reported.
const (
	KeepPinned  = "pinned"
	KeepLatest  = "latest"
	KeepLast    = "last"
	KeepHourly  = "hourly"
	KeepDaily   = "daily"
	KeepWeekly  = "weekly"
	KeepMonthly = "monthly"
	KeepYearly  = "yearly"
)

// RetainPolicy is the snapshot retention policy, with restic's semantics. Each
// field is a number of snapshots (Last) or of time buckets (the others) to keep;
// 0 switches that rule off.
type RetainPolicy struct {
	Last, Hourly, Daily, Weekly, Monthly, Yearly int
}

// RetainSnapshot is what retention needs to know about a snapshot.
type RetainSnapshot struct {
	ID        int64
	CreatedAt time.Time
	Pinned    bool
	Forgotten bool // already forgotten: takes no part and stays forgotten
}

// RetainDecision is the verdict on one snapshot and why.
type RetainDecision struct {
	ID      int64
	Keep    bool
	Reasons []string // empty when the snapshot is forgotten
}

// RetainResult lists the verdicts newest first, plus the keep and forget sets
// sorted by id. Snapshots that were already forgotten appear in none of them.
type RetainResult struct {
	Decisions []RetainDecision
	Keep      []int64
	Forget    []int64
}

// Retain applies the policy. Per rule it walks the snapshots newest to oldest and
// keeps one whenever its bucket (hour, day, ISO week, month, year, in loc) differs
// from the previous snapshot's, until it has kept N buckets. Pinned snapshots are
// always kept and use up no bucket; the newest snapshot is always kept too (the
// mass-deletion guard compares against it). A policy that keeps nothing is an
// error rather than "forget everything".
func Retain(snaps []RetainSnapshot, pol RetainPolicy, loc *time.Location) (RetainResult, error) {
	for _, n := range []int{pol.Last, pol.Hourly, pol.Daily, pol.Weekly, pol.Monthly, pol.Yearly} {
		if n < 0 {
			return RetainResult{}, errors.New("a keep_* value must not be negative")
		}
	}
	if pol == (RetainPolicy{}) {
		return RetainResult{}, errors.New("the retention policy keeps nothing: set at least one keep_* above 0")
	}
	if loc == nil {
		loc = time.UTC
	}
	var live []RetainSnapshot
	for _, s := range snaps {
		if !s.Forgotten {
			live = append(live, s)
		}
	}
	// Newest first; equal timestamps: the higher id is the newer.
	sort.Slice(live, func(i, j int) bool {
		if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
			return live[i].CreatedAt.After(live[j].CreatedAt)
		}
		return live[i].ID > live[j].ID
	})
	reasons := make([]map[string]bool, len(live))
	for i := range reasons {
		reasons[i] = map[string]bool{}
	}
	for i, s := range live {
		if s.Pinned {
			reasons[i][KeepPinned] = true
		}
		if i == 0 {
			reasons[i][KeepLatest] = true
		}
	}
	if pol.Last > 0 {
		kept := 0
		for i, s := range live {
			if s.Pinned {
				continue
			}
			if kept == pol.Last {
				break
			}
			reasons[i][KeepLast] = true
			kept++
		}
	}
	buckets := []struct {
		reason string
		n      int
		key    func(time.Time) string
	}{
		{KeepHourly, pol.Hourly, func(t time.Time) string { return t.Format("2006-01-02T15") }},
		{KeepDaily, pol.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{KeepWeekly, pol.Weekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) }},
		{KeepMonthly, pol.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
		{KeepYearly, pol.Yearly, func(t time.Time) string { return t.Format("2006") }},
	}
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		kept, last := 0, ""
		for i, s := range live {
			if s.Pinned {
				continue
			}
			if kept == b.n {
				break
			}
			k := b.key(s.CreatedAt.In(loc))
			if k == last {
				continue
			}
			last = k
			reasons[i][b.reason] = true
			kept++
		}
	}
	order := []string{KeepPinned, KeepLatest, KeepLast, KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, KeepYearly}
	var res RetainResult
	for i, s := range live {
		d := RetainDecision{ID: s.ID, Keep: len(reasons[i]) > 0}
		for _, r := range order {
			if reasons[i][r] {
				d.Reasons = append(d.Reasons, r)
			}
		}
		res.Decisions = append(res.Decisions, d)
		if d.Keep {
			res.Keep = append(res.Keep, s.ID)
		} else {
			res.Forget = append(res.Forget, s.ID)
		}
	}
	sort.Slice(res.Keep, func(i, j int) bool { return res.Keep[i] < res.Keep[j] })
	sort.Slice(res.Forget, func(i, j int) bool { return res.Forget[i] < res.Forget[j] })
	return res, nil
}
