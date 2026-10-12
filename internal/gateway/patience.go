package gateway

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// A group's patience (#1418): when every member has failed in a way that
// passes — overloaded, busy, rate limited for a moment — and nothing of a
// reply has gone to the agent, the request isn't ended on the last one's
// error. After a pause, longer each round and never shorter than a
// member's Retry-After, those members are asked again in turn, for as long
// as the group's Patience (provider.Group.Waits) lasts, counted from the
// first time every member had failed, when the agent would have had the
// error. The members take turns: the last one isn't first asked again
// alone, as the last one left of a plain provider is (passing), while
// another member is busy too. A streaming agent is kept alive with SSE
// comments through the pause, as through a held stream. A plain provider,
// which the agent picked as itself, keeps the retries passing gives the
// last one left.

// replanRounds is how many times the members are asked again, at most,
// whatever the patience left; replanLongest is the longest pause between.
const (
	replanRounds  = 6
	replanLongest = 30 * time.Second
)

// busyTry is a member that failed in a way that passes, and when it may
// be asked again: now, or when its Retry-After says.
type busyTry struct {
	c     candidate
	ready time.Time
}

// busy says whether a failure is one a member may be over in a moment:
// overloaded, unavailable, timed out, or a rate limit that isn't the
// plan's or the money's. A proxy that refused the connection, a quota used
// up or response protection's 502 isn't.
func busy(status int, body []byte) bool {
	switch status {
	case 408, 500, 502, 503, 504, 529:
		return failure(status, body) == failOther && !protectionRefused(status, body)
	case 429:
		return failure(status, body) == failRate && !creditWords.Match(wordsOf(body))
	}
	return false
}

// noteBusy adds c to the members to ask again, once, with when its vendor
// said it may be asked.
func noteBusy(list []busyTry, c candidate, header http.Header) []busyTry {
	if slices.ContainsFunc(list, func(b busyTry) bool { return b.c.restKey() == c.restKey() && b.c.effort == c.effort }) {
		return list
	}
	now := time.Now()
	return append(list, busyTry{c: c, ready: now.Add(retryAfter(header, now))})
}

// replan is, of the members that failed so in this round, those to ask
// again and the pause before: retryPause doubled each round up to
// replanLongest, and as long as the soonest Retry-After. ok is false when
// the rounds are spent, or the pause would end past by.
func replan(list []busyTry, round int, by time.Time) ([]candidate, time.Duration, bool) {
	if len(list) == 0 || round >= replanRounds {
		return nil, 0, false
	}
	now := time.Now()
	wait := min(retryPause<<(round+1), replanLongest)
	soonest := list[0].ready
	for _, b := range list[1:] {
		soonest = minTime(soonest, b.ready)
	}
	wait = max(wait, soonest.Sub(now))
	at := now.Add(wait)
	if !at.Before(by) {
		return nil, 0, false
	}
	var next, later []candidate
	for _, b := range list {
		if b.ready.After(at) {
			later = append(later, b.c) // still asking for time: after those back
			continue
		}
		next = append(next, b.c)
	}
	return append(next, later...), wait, true
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// pauseAlive waits d, keeping a streaming agent alive as a held stream
// does once the request has gone keepHeldAfter without a word. false: the
// agent went away first.
func pauseAlive(ctx context.Context, w http.ResponseWriter, kept *keptAlive, streams bool, start time.Time, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	var hw *holdWriter
	if streams && kept != nil && kept.proto != provider.Gemini {
		hw = newHoldWriter(w, true)
		hw.alive, hw.streams = kept, true
	}
	tick := time.NewTicker(min(watchEvery, max(d, time.Millisecond)))
	defer tick.Stop()
	for {
		if hw != nil && time.Since(start) >= keepHeldAfter {
			hw.keepAlive()
		}
		select {
		case <-t.C:
			return true
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
