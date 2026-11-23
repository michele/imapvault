package plan

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"time"
)

var utc = time.UTC

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, utc)
	if err != nil {
		panic(err)
	}
	return t
}

func snap(id int64, when string) RetainSnapshot { return RetainSnapshot{ID: id, CreatedAt: at(when)} }

func pinned(s RetainSnapshot) RetainSnapshot { s.Pinned = true; return s }

func TestRetain(t *testing.T) {
	tests := []struct {
		name   string
		snaps  []RetainSnapshot
		pol    RetainPolicy
		keep   []int64 // sorted ascending
		forget []int64
	}{
		{"no snapshots", nil, RetainPolicy{Last: 3}, nil, nil},
		{"one snapshot", []RetainSnapshot{snap(1, "2026-01-01 10:00")}, RetainPolicy{Last: 3}, []int64{1}, nil},
		{
			"keep_last keeps the newest N",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-01-02 10:00"), snap(3, "2026-01-03 10:00"), snap(4, "2026-01-04 10:00")},
			RetainPolicy{Last: 2}, []int64{3, 4}, []int64{1, 2},
		},
		{
			"keep_last larger than the number of snapshots",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-01-02 10:00")},
			RetainPolicy{Last: 10}, []int64{1, 2}, nil,
		},
		{
			"input order does not matter",
			[]RetainSnapshot{snap(3, "2026-01-03 10:00"), snap(1, "2026-01-01 10:00"), snap(4, "2026-01-04 10:00"), snap(2, "2026-01-02 10:00")},
			RetainPolicy{Last: 2}, []int64{3, 4}, []int64{1, 2},
		},
		{
			"hourly keeps the newest of each hour",
			[]RetainSnapshot{snap(1, "2026-01-01 10:05"), snap(2, "2026-01-01 10:50"), snap(3, "2026-01-01 11:10"), snap(4, "2026-01-01 12:30"), snap(5, "2026-01-01 12:40")},
			RetainPolicy{Hourly: 2}, []int64{3, 5}, []int64{1, 2, 4},
		},
		{
			"hourly counts buckets, not snapshots, and skips empty hours",
			[]RetainSnapshot{snap(1, "2026-01-01 01:00"), snap(2, "2026-01-01 05:00"), snap(3, "2026-01-01 09:00")},
			RetainPolicy{Hourly: 2}, []int64{2, 3}, []int64{1},
		},
		{
			"hourly separates the same hour on different days",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-01-02 10:00")},
			RetainPolicy{Hourly: 5}, []int64{1, 2}, nil,
		},
		{
			"daily keeps the newest of each day",
			[]RetainSnapshot{snap(1, "2026-01-01 08:00"), snap(2, "2026-01-01 20:00"), snap(3, "2026-01-02 08:00"), snap(4, "2026-01-03 08:00")},
			RetainPolicy{Daily: 2}, []int64{3, 4}, []int64{1, 2},
		},
		{
			"daily 3 over four days",
			[]RetainSnapshot{snap(1, "2026-01-01 08:00"), snap(2, "2026-01-01 20:00"), snap(3, "2026-01-02 08:00"), snap(4, "2026-01-03 08:00")},
			RetainPolicy{Daily: 3}, []int64{2, 3, 4}, []int64{1},
		},
		{
			// 2026-01-05 is a Monday, so Jan 5 starts ISO week 2.
			"weekly uses ISO weeks that start on Monday",
			[]RetainSnapshot{snap(1, "2026-01-03 10:00"), snap(2, "2026-01-04 10:00"), snap(3, "2026-01-05 10:00"), snap(4, "2026-01-11 10:00")},
			RetainPolicy{Weekly: 2}, []int64{2, 4}, []int64{1, 3},
		},
		{
			"weekly across the year boundary: 2026-12-31 and 2027-01-01 are both in 2026-W53",
			[]RetainSnapshot{snap(1, "2026-12-31 10:00"), snap(2, "2027-01-01 10:00"), snap(3, "2027-01-04 10:00")},
			RetainPolicy{Weekly: 2}, []int64{2, 3}, []int64{1},
		},
		{
			"monthly keeps the newest of each month",
			[]RetainSnapshot{snap(1, "2026-01-10 10:00"), snap(2, "2026-01-20 10:00"), snap(3, "2026-02-10 10:00"), snap(4, "2026-03-10 10:00")},
			RetainPolicy{Monthly: 2}, []int64{3, 4}, []int64{1, 2},
		},
		{
			"monthly separates the same month of different years",
			[]RetainSnapshot{snap(1, "2025-03-10 10:00"), snap(2, "2026-03-10 10:00")},
			RetainPolicy{Monthly: 2}, []int64{1, 2}, nil,
		},
		{
			"yearly keeps the newest of each year",
			[]RetainSnapshot{snap(1, "2024-01-10 10:00"), snap(2, "2024-12-10 10:00"), snap(3, "2025-06-10 10:00"), snap(4, "2026-02-10 10:00")},
			RetainPolicy{Yearly: 3}, []int64{2, 3, 4}, []int64{1},
		},
		{
			"rules add up",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-02-01 10:00"), snap(3, "2026-03-01 10:00"), snap(4, "2026-03-02 10:00"), snap(5, "2026-03-02 11:00")},
			RetainPolicy{Last: 1, Monthly: 2}, []int64{2, 5}, []int64{1, 3, 4},
		},
		{
			"a pinned snapshot is kept outside every bucket",
			[]RetainSnapshot{pinned(snap(1, "2026-01-01 10:00")), snap(2, "2026-01-02 10:00"), snap(3, "2026-01-03 10:00")},
			RetainPolicy{Last: 1}, []int64{1, 3}, []int64{2},
		},
		{
			"a pinned snapshot does not use up a bucket",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), pinned(snap(2, "2026-01-02 10:00")), snap(3, "2026-01-03 10:00")},
			RetainPolicy{Daily: 2}, []int64{1, 2, 3}, nil,
		},
		{
			"the latest snapshot is always kept",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-01-02 10:00")},
			RetainPolicy{Yearly: 1}, []int64{2}, []int64{1},
		},
		{
			"equal timestamps: the higher id is the newer",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), snap(2, "2026-01-01 10:00")},
			RetainPolicy{Last: 1}, []int64{2}, []int64{1},
		},
		{
			"already forgotten snapshots take no part and stay forgotten",
			[]RetainSnapshot{forgotten(snap(1, "2026-01-01 10:00")), snap(2, "2026-01-02 10:00"), snap(3, "2026-01-03 10:00")},
			RetainPolicy{Last: 2}, []int64{2, 3}, nil,
		},
		{
			"a forgotten snapshot does not use up keep_last",
			[]RetainSnapshot{snap(1, "2026-01-01 10:00"), forgotten(snap(2, "2026-01-02 10:00")), snap(3, "2026-01-03 10:00")},
			RetainPolicy{Last: 2}, []int64{1, 3}, nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Retain(tc.snaps, tc.pol, utc)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(nilIfEmpty(res.Keep), nilIfEmpty(tc.keep)) {
				t.Errorf("keep = %v, want %v", res.Keep, tc.keep)
			}
			if !reflect.DeepEqual(nilIfEmpty(res.Forget), nilIfEmpty(tc.forget)) {
				t.Errorf("forget = %v, want %v", res.Forget, tc.forget)
			}
		})
	}
}

func forgotten(s RetainSnapshot) RetainSnapshot { s.Forgotten = true; return s }

func nilIfEmpty(s []int64) []int64 {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestRetainRejectsAPolicyThatKeepsNothing(t *testing.T) {
	_, err := Retain([]RetainSnapshot{snap(1, "2026-01-01 10:00")}, RetainPolicy{}, utc)
	if err == nil {
		t.Fatal("an all-zero policy must be an error, not \"forget everything\"")
	}
}

func TestRetainRejectsNegativeCounts(t *testing.T) {
	_, err := Retain([]RetainSnapshot{snap(1, "2026-01-01 10:00")}, RetainPolicy{Last: 1, Daily: -1}, utc)
	if err == nil {
		t.Fatal("a negative keep must be an error")
	}
}

func TestRetainReasons(t *testing.T) {
	snaps := []RetainSnapshot{pinned(snap(1, "2026-01-01 10:00")), snap(2, "2026-01-02 10:00"), snap(3, "2026-01-02 11:00")}
	res, err := Retain(snaps, RetainPolicy{Last: 1, Daily: 1}, utc)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64][]string{}
	for _, d := range res.Decisions {
		got[d.ID] = d.Reasons
	}
	want := map[int64][]string{
		1: {"pinned"},
		2: nil,
		3: {"latest", "last", "daily"},
	}
	if !reflect.DeepEqual(nilIfEmptyStr(got[1]), want[1]) || !reflect.DeepEqual(nilIfEmptyStr(got[2]), want[2]) || !reflect.DeepEqual(got[3], want[3]) {
		t.Errorf("reasons = %v, want %v", got, want)
	}
	// Decisions are newest first.
	var ids []int64
	for _, d := range res.Decisions {
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []int64{3, 2, 1}) {
		t.Errorf("decision order = %v, want newest first", ids)
	}
}

func nilIfEmptyStr(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestRetainUsesTheGivenLocation(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 23:30 UTC on Jan 1 is 00:30 on Jan 2 in Rome. Snapshots 1 and 2 share a day
	// in UTC; in Rome, 2 and 3 share one.
	snaps := []RetainSnapshot{
		{ID: 1, CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		{ID: 2, CreatedAt: time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)},
		{ID: 3, CreatedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)},
	}
	inUTC, _ := Retain(snaps, RetainPolicy{Daily: 10}, time.UTC)
	inRome, _ := Retain(snaps, RetainPolicy{Daily: 10}, rome)
	if !reflect.DeepEqual(inUTC.Keep, []int64{2, 3}) {
		t.Errorf("UTC keep = %v", inUTC.Keep)
	}
	// Rome: 1 is Jan 1 (13:00); 2 and 3 are both Jan 2, and 3 is the newer.
	if !reflect.DeepEqual(inRome.Keep, []int64{1, 3}) {
		t.Errorf("Rome keep = %v, want [1 3]", inRome.Keep)
	}
}

func TestRetainDaylightSavingDay(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 2026-03-29 has 23 hours in Rome. Hourly buckets must still be distinct
	// per real hour, and the day must be one bucket.
	var snaps []RetainSnapshot
	for h := 0; h < 24; h++ {
		snaps = append(snaps, RetainSnapshot{ID: int64(h + 1), CreatedAt: time.Date(2026, 3, 28, 23, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)})
	}
	day, _ := Retain(snaps, RetainPolicy{Daily: 1}, rome)
	// The newest snapshot (id 24) is the only one kept for the newest day, and it is also the latest.
	if !slices.Contains(day.Keep, 24) {
		t.Fatalf("keep = %v, want it to hold 24", day.Keep)
	}
	hours, _ := Retain(snaps, RetainPolicy{Hourly: 24}, rome)
	if len(hours.Keep) != 24 {
		t.Errorf("hourly kept %d of 24 distinct hours, want all", len(hours.Keep))
	}
}

// TestRetainProperties checks invariants over random snapshot sets.
func TestRetainProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	base := at("2025-01-01 00:00")
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(40)
		var snaps []RetainSnapshot
		when := base
		for i := 0; i < n; i++ {
			when = when.Add(time.Duration(rng.Intn(60*24*9)) * time.Minute)
			s := RetainSnapshot{ID: int64(i + 1), CreatedAt: when, Pinned: rng.Intn(10) == 0}
			snaps = append(snaps, s)
		}
		pol := RetainPolicy{Last: rng.Intn(4), Hourly: rng.Intn(4), Daily: rng.Intn(5), Weekly: rng.Intn(5), Monthly: rng.Intn(5), Yearly: rng.Intn(3)}
		if pol == (RetainPolicy{}) {
			pol.Last = 1
		}
		res, err := Retain(snaps, pol, utc)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Keep)+len(res.Forget) != n {
			t.Fatalf("iter %d: keep+forget = %d, want %d", iter, len(res.Keep)+len(res.Forget), n)
		}
		// Pinned and the latest are always kept.
		for _, s := range snaps {
			if s.Pinned && !slices.Contains(res.Keep, s.ID) {
				t.Fatalf("iter %d: pinned snapshot %d was forgotten", iter, s.ID)
			}
		}
		if !slices.Contains(res.Keep, int64(n)) {
			t.Fatalf("iter %d: the latest snapshot was forgotten", iter)
		}
		// Raising any keep never forgets more.
		more := pol
		more.Daily++
		more.Monthly++
		more.Last++
		res2, err := Retain(snaps, more, utc)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range res.Keep {
			if !slices.Contains(res2.Keep, id) {
				t.Fatalf("iter %d: raising the policy forgot snapshot %d", iter, id)
			}
		}
		// Applying the policy again to what was kept, with the forgotten marked, changes nothing.
		again := make([]RetainSnapshot, len(snaps))
		copy(again, snaps)
		for i := range again {
			again[i].Forgotten = slices.Contains(res.Forget, again[i].ID)
		}
		res3, err := Retain(again, pol, utc)
		if err != nil {
			t.Fatal(err)
		}
		if len(res3.Forget) != 0 {
			t.Fatalf("iter %d: applying the policy twice forgot %v more", iter, res3.Forget)
		}
	}
}
