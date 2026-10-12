package usage

import (
	"testing"
	"time"
)

// A group's speed says how many replies it is of, and a group none of whose
// replies was measured says the least they wrote at (Fallmonkey on Discord:
// gpt-6.1-sol showed a tok/s from the one reply of 260 that called no tool,
// beside "260 requests", while Claude Code, with dozens of floors on the
// Requests tab, showed none). Breakdown and the sums count the measured
// replies (DecodeN) and the floors (FloorOut over FloorMs, FloorN) apart.
func TestGroupSpeedSaysHowManyRepliesItIsOf(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	var rows []Row
	// sol: 260 Codex calls read from its session files; only a turn of one
	// call is timed, 500 tokens over the 4 s after the first, 125 tok/s; the
	// rest, calls in turns of several, wrote 1,500 tokens over 30 s each, a
	// floor of 50 tok/s
	for i := 0; i < 260; i++ {
		r := Row{Source: "log", Record: Record{Time: at, Agent: "codex", Model: "gpt-6.1-sol", Input: 9000, Output: 1500, Millis: 30000}}
		if i == 0 {
			r.Output, r.Millis, r.TTFT = 500, 5000, 1000
		}
		rows = append(rows, r)
	}
	// Claude Code: 40 calls from its session file, none timed; 30 wrote
	// 2,000 tokens over 20 s (a floor of 100 tok/s), 10 are too short to tell
	for i := 0; i < 40; i++ {
		r := Row{Source: "log", Record: Record{Time: at, Agent: "claude", Model: "claude-opus-5-5", Input: 5000, Output: 2000, Millis: 20000}}
		if i >= 30 {
			r.Output = 300
		}
		rows = append(rows, r)
	}
	// a failed Claude Code call and one through the gateway tell no floor
	rows = append(rows,
		Row{Source: "log", Record: Record{Time: at, Agent: "claude", Model: "claude-opus-5-5", Output: 2000, Millis: 20000, Error: "overloaded"}},
		Row{Record: Record{Time: at, Agent: "claude", Model: "claude-opus-5-5", Output: 2000, Millis: 20000, Status: 200}},
	)
	by := map[string]Share{}
	for _, s := range Breakdown(rows, "agent") {
		by[s.ID] = s
	}
	sol := by["codex"]
	if sol.Calls != 260 || sol.DecodeN != 1 || sol.Speed() != 125 || sol.FloorN != 259 || sol.Floor() != 50 {
		t.Fatalf("sol: %d calls, %d measured at %v tok/s, %d floors at %v; want 260, 1 at 125, 259 at 50", sol.Calls, sol.DecodeN, sol.Speed(), sol.FloorN, sol.Floor())
	}
	cc := by["claude"]
	if cc.Calls != 42 || cc.DecodeN != 0 || cc.Speed() != 0 || cc.FloorN != 30 || cc.FloorOut != 60000 || cc.FloorMs != 600000 || cc.Floor() != 100 {
		t.Fatalf("Claude Code: %d calls, %d measured at %v, %d floors %d/%d ms at %v; want 42, none, 30 at 100", cc.Calls, cc.DecodeN, cc.Speed(), cc.FloorN, cc.FloorOut, cc.FloorMs, cc.Floor())
	}
	// the page's own sums count both the same way
	var sum Totals
	for _, r := range rows {
		sum.addRow(r)
	}
	if sum.DecodeN != 1 || sum.FloorN != 289 || sum.Speed() != 125 {
		t.Fatalf("sum: %+v", sum)
	}
	// a timed Claude Code row tells its own speed, not a floor
	timed := Row{Source: "log", Record: Record{Agent: "claude", Output: 2000, Millis: 20000, TTFT: 4000}}
	if n, ms := timed.Floor(); ms != 0 || n != 0 {
		t.Fatalf("timed row's floor %d/%d", n, ms)
	}
	// records through the gateway count their measured replies too
	recs := []Record{
		{Time: at, Provider: "openai", Model: "gpt-6.1-sol", Output: 1200, Millis: 15000, TTFT: 3000, Status: 200},
		{Time: at, Provider: "openai", Model: "gpt-6.1-sol", Output: 1200, Millis: 15000, Status: 200},
	}
	m := summarize(Today, at.Add(time.Hour), recs).Models[0]
	if m.Calls != 2 || m.DecodeN != 1 || m.Speed() != 100 {
		t.Fatalf("summarized: %+v", m.Totals)
	}
}

// Codex rows tell a floor too (Fallmonkey on Discord): a call in a turn of
// several, timed from what asked for it, by the same rule as Claude Code's;
// a turn of one call that reasoned, whose first text isn't in the file, by
// its answer over the time after its first token, never its whole output
// over it, which counts reasoning written before the first token.
func TestCodexRowsTellAFloor(t *testing.T) {
	row := func(out, reasoning int, ms, ttft int64) Row {
		return Row{Source: "log", Record: Record{Agent: "codex", Model: "gpt-6-luna", Output: out, Reasoning: reasoning, Millis: ms, TTFT: ttft}}
	}
	for _, c := range []struct {
		name string
		r    Row
		n    int
		ms   int64
	}{
		// the gateway measured 415 tok/s on this one's answer; 272 over 827 ms is 329
		{"one call that reasoned", row(496, 224, 11055, 10228), 272, 827},
		{"one call that reasoned, all of it after its first token", row(1200, 200, 12000, 2000), 1000, 10000},
		{"one call that didn't reason tells its own speed", row(500, 0, 5000, 1000), 0, 0},
		{"one call whose answer came too fast to tell", row(500, 100, 2050, 2000), 0, 0},
		{"one call that only reasoned", row(300, 300, 9000, 2000), 0, 0},
		{"a call of several, 1,500 over 30 s", row(1500, 600, 30000, 0), 1500, 30000},
		{"a call of several too short to tell", row(800, 100, 20000, 0), 0, 0},
	} {
		n, ms := c.r.Floor()
		if n != c.n || ms != c.ms {
			t.Errorf("%s: floor %d/%d ms, want %d/%d", c.name, n, ms, c.n, c.ms)
		}
	}
	// through the gateway a Codex row is measured, never a floor
	gw := row(496, 224, 11055, 10228)
	gw.Source = ""
	if n, ms := gw.Floor(); n != 0 || ms != 0 {
		t.Errorf("gateway row: floor %d/%d", n, ms)
	}
}
