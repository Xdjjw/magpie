package access

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLimitWindow(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ period, now, start, reset string }{
		{"day", "2026-10-03 23:59", "2026-10-03 00:00", "2026-10-04 00:00"},
		{"day", "2026-12-31 00:00", "2026-12-31 00:00", "2027-01-01 00:00"},
		{"week", "2026-10-03 12:00", "2026-09-28 00:00", "2026-10-05 00:00"}, // a Saturday: from Monday
		{"week", "2026-10-04 23:00", "2026-09-28 00:00", "2026-10-05 00:00"}, // Sunday is the week's last day
		{"week", "2026-10-05 00:00", "2026-10-05 00:00", "2026-10-12 00:00"},
		{"month", "2026-10-31 18:00", "2026-10-01 00:00", "2026-11-01 00:00"},
		{"month", "2026-12-15 08:00", "2026-12-01 00:00", "2027-01-01 00:00"},
	} {
		start, reset := Window(c.period, at(c.now))
		if !start.Equal(at(c.start)) || !reset.Equal(at(c.reset)) {
			t.Errorf("%s at %s: %s – %s", c.period, c.now, start, reset)
		}
	}
}

func TestLimitValid(t *testing.T) {
	if l, err := (&Limit{Period: "day"}).Valid(); l != nil || err != nil {
		t.Fatal("no cap is no limit", l, err)
	}
	if l, err := (*Limit)(nil).Valid(); l != nil || err != nil {
		t.Fatal(l, err)
	}
	if l, err := (&Limit{Tokens: 5}).Valid(); err != nil || l.Period != "day" {
		t.Fatal("a period is a day unless said", l, err)
	}
	if l, err := (&Limit{Period: "week", Days: 9, Tokens: 5}).Valid(); err != nil || l.Days != 0 {
		t.Fatal("a calendar period has no days", l, err)
	}
	if l, err := (&Limit{Period: "days", Days: 10, Cost: 800}).Valid(); err != nil || l.Days != 10 {
		t.Fatal(l, err)
	}
	for _, bad := range []Limit{{Period: "hour", Tokens: 1}, {Period: "day", Tokens: -1}, {Period: "day", Cost: -2},
		{Period: "days", Tokens: 1}, {Period: "days", Days: -3, Tokens: 1}, {Period: "days", Days: MaxDays + 1, Tokens: 1}} {
		if _, err := bad.Valid(); err == nil {
			t.Error("accepted", bad)
		}
	}
}

// An N-day cycle (#1509) runs from when the limit was set, each next one
// N×24h of absolute time after the last: the same instants whatever the
// time zone the clock is read in, and a summer-time change doesn't move
// them.
func TestLimitDaysCycle(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	// set at 15:30 two days before New York's clocks went back (2026-11-01)
	since := time.Date(2026, 10, 30, 15, 30, 0, 0, ny)
	day := 24 * time.Hour
	l := &Limit{Period: "days", Days: 10, Cost: 800, Since: since}
	for _, c := range []struct {
		now         time.Time
		start, rest time.Duration // from since
	}{
		{since, 0, 10 * day},
		{since.Add(10*day - time.Nanosecond), 0, 10 * day},
		{since.Add(10 * day), 10 * day, 20 * day},
		{since.Add(57*day + 3*time.Hour), 50 * day, 60 * day},
	} {
		for _, loc := range []*time.Location{ny, time.UTC, time.FixedZone("UTC+8", 8*3600)} {
			start, reset := l.Window(c.now.In(loc))
			if !start.Equal(since.Add(c.start)) || !reset.Equal(since.Add(c.rest)) {
				t.Errorf("at %s: %s – %s, want %s – %s", c.now.In(loc), start, reset, since.Add(c.start), since.Add(c.rest))
			}
			if start.Location() != loc {
				t.Errorf("the window is said in %s, not the clock's %s", start.Location(), loc)
			}
		}
	}
	// across the change the day is 24 hours, so the wall clock moves
	one := &Limit{Period: "days", Days: 1, Tokens: 5, Since: since}
	start, reset := one.Window(since.Add(2*day + time.Hour))
	if reset.Sub(start) != day || reset.Hour() != 14 || reset.Minute() != 30 {
		t.Errorf("across summer time: %s – %s", start, reset)
	}
	// the clock set back before it was set: the first cycle
	if start, reset := l.Window(since.Add(-time.Hour)); !start.Equal(since) || !reset.Equal(since.Add(10*day)) {
		t.Errorf("before since: %s – %s", start, reset)
	}
	// written by hand without a since: from the Unix epoch, the same on every read
	hand := &Limit{Period: "days", Days: 7, Tokens: 5}
	a, _ := hand.Window(since)
	b, _ := hand.Window(since.Add(time.Hour))
	if !a.Equal(b) || a.Sub(time.Unix(0, 0))%(7*day) != 0 {
		t.Errorf("hand-written cycle: %s, %s", a, b)
	}
}

// A calendar limit reset (#1509) counts from the reset to the window's
// own end; a reset in an earlier window doesn't move the next one.
func TestLimitCalendarReset(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, loc) // a Wednesday
	reset := time.Date(2026, 10, 7, 9, 15, 0, 0, loc)
	l := &Limit{Period: "week", Tokens: 5, Since: reset}
	start, end := l.Window(now)
	if !start.Equal(reset) || !end.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, loc)) {
		t.Fatalf("reset this week: %s – %s", start, end)
	}
	start, end = l.Window(now.AddDate(0, 0, 7))
	if !start.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, loc)) || !end.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, loc)) {
		t.Fatalf("the week after: %s – %s", start, end)
	}
	// never reset: the calendar's
	l.Since = time.Time{}
	if start, _ := l.Window(now); !start.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, loc)) {
		t.Fatal(start)
	}
}

// Setting a limit: an N-day cycle starts when it is set and keeps its
// start while only its caps change; a new period or cycle starts over; a
// since the caller sends is never kept; a key from before #1509 keeps its
// calendar window untouched; Reset counts from now, tokens and cost both.
func TestLimitSetAndReset(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clock := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	Now = func() time.Time { return clock }
	t.Cleanup(func() { Now = time.Now })
	// a key as an older magpie kept it
	old := `[{"id":"k1","name":"Old","secret":"sk-magpie-key-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","limit":{"period":"week","tokens":5000}}]`
	if err := os.MkdirAll(strings.TrimSuffix(Path(), "caller-keys.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	limitOf := func() *Limit {
		t.Helper()
		keys, err := List()
		if err != nil || len(keys) != 1 {
			t.Fatal(keys, err)
		}
		return keys[0].Limit
	}
	set := func(l *Limit) {
		t.Helper()
		if _, err := Update("limit-key", Change{Key: "k1", Limit: l}); err != nil {
			t.Fatal(err)
		}
	}
	if l := limitOf(); l.Period != "week" || l.Tokens != 5000 || !l.Since.IsZero() {
		t.Fatalf("an old limit read as %+v", l)
	}
	// renamed: the limit is as it was, byte for byte
	if _, err := Update("rename-key", Change{Key: "k1", Name: "Older"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Path()); strings.Contains(string(b), "since") || strings.Contains(string(b), "days") {
		t.Fatalf("a rename wrote %s", b)
	}
	set(&Limit{Period: "week", Tokens: 6000})
	if l := limitOf(); !l.Since.IsZero() {
		t.Fatalf("a calendar limit's new cap gave it a since: %+v", l)
	}
	set(&Limit{Period: "days", Days: 10, Cost: 800, Since: clock.Add(-99 * time.Hour)})
	if l := limitOf(); l.Period != "days" || l.Days != 10 || !l.Since.Equal(clock) {
		t.Fatalf("a 10-day cycle set at %s: %+v", clock, l)
	}
	set1 := clock
	clock = clock.Add(50 * time.Hour)
	set(&Limit{Period: "days", Days: 10, Cost: 900, Tokens: 1000})
	if l := limitOf(); !l.Since.Equal(set1) || l.Cost != 900 {
		t.Fatalf("a new cap restarted the cycle: %+v", l)
	}
	set(&Limit{Period: "days", Days: 7, Cost: 900})
	if l := limitOf(); !l.Since.Equal(clock) {
		t.Fatalf("a new cycle length kept the old start: %+v", l)
	}
	clock = clock.Add(time.Hour)
	if _, err := Update("reset-limit-key", Change{Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if l := limitOf(); !l.Since.Equal(clock) || l.Days != 7 || l.Cost != 900 {
		t.Fatalf("reset: %+v", l)
	}
	// the store reloads as a restart would
	if who, ok := Authenticate("sk-magpie-key-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !ok || !who.Limit.Since.Equal(clock) {
		t.Fatalf("after a reload: %+v", who.Limit)
	}
	set(&Limit{Period: "month", Tokens: 10})
	if l := limitOf(); !l.Since.IsZero() || l.Days != 0 {
		t.Fatalf("back to a calendar month: %+v", l)
	}
	clock = clock.Add(time.Hour)
	if _, err := Update("reset-limit-key", Change{Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if l := limitOf(); !l.Since.Equal(clock) || l.Period != "month" {
		t.Fatalf("a calendar reset: %+v", l)
	}
	set(nil)
	if _, err := Update("reset-limit-key", Change{Key: "k1"}); err == nil {
		t.Fatal("reset a key with no limit")
	}
	if _, err := Update("reset-limit-key", Change{Key: "nope"}); err == nil {
		t.Fatal("reset a missing key")
	}
}
