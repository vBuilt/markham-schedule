package scraper

import (
	"context"
	"sort"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

// regBatch is one category's freshly converted sessions with their EventIds.
type regBatch struct {
	sessions []schedule.Session // modified in place
	eventIDs []string
}

// maxRegFailures stops lookups for the rest of a refresh after this many
// consecutive failures, so a blocked or broken site isn't hammered.
const maxRegFailures = 5

// addRegistration fills Session.Registration from each course's saved rule,
// first fetching rules for courses that have none (or a stale one), soonest
// sessions first, within the per-refresh budget. Only a cancelled context is
// returned as an error; lookup failures leave those sessions' dates unknown.
func (s *Scraper) addRegistration(ctx context.Context, now time.Time, batches []regBatch, stats *schedule.RegStats) error {
	rules, err := s.st.RegRules(ctx)
	if err != nil {
		s.log.Warn("load registration rules", "err", err)
		rules = map[string]schedule.RegRule{}
	}

	// Earliest occurrence of each course: the sample used to learn its rule.
	first := map[string]time.Time{}
	for _, b := range batches {
		for i, sess := range b.sessions {
			id := b.eventIDs[i]
			if t, ok := first[id]; !ok || sess.Start.Before(t) {
				first[id] = sess.Start
			}
		}
	}
	var missing, stale []string
	for id := range first {
		r, ok := rules[id]
		switch {
		case !ok:
			missing = append(missing, id)
		case now.Sub(r.Fetched) > s.cfg.RegMaxAge:
			stale = append(stale, id)
		}
	}
	bySoonest := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool {
			if !first[ids[i]].Equal(first[ids[j]]) {
				return first[ids[i]].Before(first[ids[j]])
			}
			return ids[i] < ids[j]
		})
	}
	bySoonest(missing)
	bySoonest(stale)
	queue := append(missing, stale...)

	budget := s.cfg.RegLookups
	if budget < 0 {
		budget = 0
	}
	looked, failed, streak := 0, 0, 0
	for _, id := range queue {
		if looked >= budget || streak >= maxRegFailures {
			break
		}
		looked++
		start := first[id]
		res, mem, pub, err := s.src.Registration(ctx, id, start.In(s.cfg.Loc).Format("20060102"), s.cfg.Loc)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			streak++
			s.log.Warn("registration lookup failed", "event", id, "err", err)
			continue
		}
		streak = 0
		rule := schedule.DeriveRegRule(res, mem, pub, start)
		rule.Fetched = now
		if err := s.st.SaveRegRule(ctx, id, rule); err != nil {
			s.log.Warn("save registration rule", "event", id, "err", err)
		}
		rules[id] = rule
	}
	unknown := 0
	for id := range first {
		if _, ok := rules[id]; !ok {
			unknown++
		}
	}
	*stats = schedule.RegStats{Courses: len(first), LookedUp: looked, Failed: failed, Unknown: unknown}
	if len(queue) > 0 {
		s.log.Info("registration lookups", "looked_up", looked, "failed", failed,
			"new_courses", len(missing), "stale_courses", len(stale), "still_unknown", unknown)
	}

	for _, b := range batches {
		for i := range b.sessions {
			if r, ok := rules[b.eventIDs[i]]; ok {
				b.sessions[i].Registration = r.Apply(b.sessions[i].Start)
			}
		}
	}
	return nil
}
