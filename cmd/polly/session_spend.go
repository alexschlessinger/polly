package main

import (
	"sync"

	"github.com/alexschlessinger/pollytool/llm"
)

// sessionSpend totals what a session has cost since this process opened it:
// its settled turns, the turn in flight, and every model call its swarm
// members made. Members report from their own goroutines, so all access goes
// through the mutex. Spend that could be neither billed nor priced counts as
// nothing and marks the total as an estimate.
type sessionSpend struct {
	mu        sync.Mutex
	committed turnCost
	turn      turnCost
	// missing records spend that could be neither billed nor priced.
	missing bool
}

// plus sums two costs: known once either part is, estimated when either is.
func (c turnCost) plus(o turnCost) turnCost {
	return turnCost{usd: c.usd + o.usd, known: c.known || o.known, estimated: c.estimated || o.estimated}
}

// add counts one finished model call.
func (s *sessionSpend) add(c turnCost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addLocked(c)
}

func (s *sessionSpend) addLocked(c turnCost) {
	if c.known {
		s.committed = s.committed.plus(c)
	} else {
		s.missing = true
	}
}

// observeTurn replaces the in-flight parent turn's cost with its latest value.
func (s *sessionSpend) observeTurn(c turnCost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = c
}

// finishTurn counts a parent turn's final cost in place of its in-flight
// value. spent reports whether the turn used any tokens, so a turn that never
// reached the model does not make the total an estimate.
func (s *sessionSpend) finishTurn(c turnCost, spent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn = turnCost{}
	if c.known || spent {
		s.addLocked(c)
	}
}

// total returns the session's spend so far, including the turn in flight.
func (s *sessionSpend) total() turnCost {
	if s == nil {
		return turnCost{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.committed.plus(s.turn)
	t.estimated = t.estimated || t.known && s.missing
	return t
}

// memberSpend prices a swarm member's model calls into the parent session's
// spend: the provider-billed cost when one is reported, otherwise the
// member model's advertised rates. Rates are looked up on the first call that
// needs them. A member's callbacks run on one goroutine at a time.
type memberSpend struct {
	spend  *sessionSpend
	rates  func() turnRates
	loaded bool
	cached turnRates
	last   llm.UsageUpdate
}

func (m *memberSpend) progress(u llm.UsageUpdate) {
	m.last = u
}

func (m *memberSpend) iteration(_, in, out int) {
	last := m.last
	m.last = llm.UsageUpdate{}
	switch {
	case last.CostReported:
		m.spend.add(turnCost{usd: last.ReportedCostUSD, known: true})
	case in <= 0 && out <= 0:
	default:
		if !m.loaded {
			m.cached, m.loaded = m.rates(), true
		}
		if !m.cached.known {
			m.spend.add(turnCost{})
			return
		}
		m.spend.add(turnCost{usd: m.cached.cost(in, out, last.CacheReadInputTokens, last.CacheWriteInputTokens), known: true, estimated: true})
	}
}
