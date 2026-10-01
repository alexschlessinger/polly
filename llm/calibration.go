package llm

import "sync"

// Calibration keeps what providers have reported about each model route's
// requests (a model, with the host or base URL a request pins, since
// endpoints serving one model differ in window and count): the context
// window and output reserve a rejection stated, or, when it stated neither,
// how much input it refused; and, for each conversation on the route, how
// the provider's count of a request's input tokens compares with the agent's
// estimate. The ratio is the conversation's rather than the route's because
// it depends on what the conversation holds: dense tool output counts above
// its estimate, long reasoning below it, and a swarm member's ratio must not
// size its parent's next request. A conversation is its request's
// CacheSessionID; requests without one share the route's ratio. An agent
// sizes every request by it, so agents that share one start a conversation
// from its last request, not from the estimate alone. It also keeps where
// each conversation's projection has compacted and omitted up to, so a run
// of the same conversation by another agent, such as a swarm member's next
// slice, starts from it. It is safe for concurrent use.
type Calibration struct {
	mu     sync.Mutex
	models map[string]modelCalibration
	// ratios is the provider's count of a request's input tokens per token
	// the agent estimated for it, as last reported, by conversation (see
	// calibrationScope); absent until then.
	ratios map[string]float64
	// fronts is where each conversation's projection, by its cache session
	// id, has compacted and omitted up to (see projectionFronts).
	fronts map[string]projectionFronts
}

// modelCalibration is what rejections taught about a route.
type modelCalibration struct {
	// window is the least context window a rejection stated, and output
	// the output tokens the last rejection counted against it.
	window, output int
	// limit bounds the input, in provider tokens, below what a rejection
	// that stated no window refused.
	limit int
}

// NewCalibration returns an empty calibration; the zero value is one too.
func NewCalibration() *Calibration {
	return &Calibration{models: map[string]modelCalibration{}, ratios: map[string]float64{}, fronts: map[string]projectionFronts{}}
}

// ready makes the maps the zero value lacks; c.mu is held.
func (c *Calibration) ready() {
	if c.models == nil {
		c.models = map[string]modelCalibration{}
	}
	if c.ratios == nil {
		c.ratios = map[string]float64{}
	}
	if c.fronts == nil {
		c.fronts = map[string]projectionFronts{}
	}
}

// heldFronts is where the projection of the conversation id names has
// compacted and omitted up to.
func (c *Calibration) heldFronts(id string) projectionFronts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fronts[id]
}

// keepFronts records where the projection of the conversation id names has
// compacted and omitted up to.
func (c *Calibration) keepFronts(id string, fronts projectionFronts) {
	c.mu.Lock()
	c.ready()
	c.fronts[id] = fronts
	c.mu.Unlock()
}

// The ratio is bounded so a report that counts something the estimate never
// sees, such as a provider's own hidden prompt on a tiny request, cannot
// shrink a budget to nothing, nor one that counts little inflate it past use.
const (
	minRatio = 0.25
	maxRatio = 4.0
)

// Ratio is the provider's count of req's input tokens per estimated token,
// as req's conversation last learned it; 1 until a provider has reported one.
func (c *Calibration) Ratio(req *CompletionRequest) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scale(calibrationScope(req))
}

// calibrationRoute is what req's route facts are kept by: its model, and
// the host and base URL it pins, if any.
func calibrationRoute(req *CompletionRequest) string {
	if req.ModelHost == "" && req.BaseURL == "" {
		return req.Model
	}
	return req.Model + "\x00" + req.ModelHost + "\x00" + req.BaseURL
}

// calibrationScope is what req's ratio is kept by: its route, within the
// conversation its CacheSessionID names, or the route alone without one.
func calibrationScope(req *CompletionRequest) string {
	return calibrationRoute(req) + "\x00" + req.CacheSessionID
}

// scale is the ratio kept for scope, 1 when none is; c.mu is held.
func (c *Calibration) scale(scope string) float64 {
	if ratio := c.ratios[scope]; ratio > 0 {
		return ratio
	}
	return 1
}

// learnUsage keeps the ratio of what the provider reported req's input
// costing to what the agent estimated it at, for req's conversation.
func (c *Calibration) learnUsage(req *CompletionRequest, reported, estimated int) {
	if reported <= 0 || estimated <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready()
	c.ratios[calibrationScope(req)] = boundRatio(float64(reported) / float64(estimated))
}

// overflowStep is how far a rejection that states no count is taken to have
// missed by: the retry asks a quarter less of what the rejection left unknown.
const overflowStep = 1.25

// learnOverflow keeps what a rejection of req, estimated at estimated tokens
// and sent asking for req.MaxTokens of output, said: the window and the
// output counted against it, for the route, and the ratio its count of the
// input implies, for req's conversation. What it left unstated is bounded by
// what it refused. With the window stated but not the count, the provider
// counted more input than the window left room for, so the ratio rises at
// least that far, and a step past it. With neither stated, the input it
// counted, or would have by the ratio, was over the limit, so the limit
// falls a step below it.
func (c *Calibration) learnOverflow(req *CompletionRequest, overflow *ContextOverflowError, estimated int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready()
	route, scope := calibrationRoute(req), calibrationScope(req)
	m := c.models[route]
	if overflow.Window > 0 && (m.window == 0 || overflow.Window < m.window) {
		m.window = overflow.Window
	}
	if overflow.Output > 0 {
		m.output = overflow.Output
	}
	if estimated <= 0 {
		c.models[route] = m
		return
	}
	ratio := c.scale(scope)
	counted := float64(estimated) * ratio
	if overflow.Input > 0 {
		counted = float64(overflow.Input)
		ratio = boundRatio(counted / float64(estimated))
		c.ratios[scope] = ratio
	}
	switch {
	case overflow.Window > 0 && overflow.Input == 0:
		reserve := req.MaxTokens
		if overflow.Output > 0 {
			reserve = overflow.Output
		}
		least := ratio
		if room := overflow.Window - reserve; room > 0 {
			least = max(least, float64(room)/float64(estimated))
		}
		c.ratios[scope] = boundRatio(least * overflowStep)
	case overflow.Window == 0:
		if limit := int(counted / overflowStep); m.limit == 0 || limit < m.limit {
			m.limit = max(1, limit)
		}
	}
	c.models[route] = m
}

func boundRatio(ratio float64) float64 {
	return min(maxRatio, max(minRatio, ratio))
}

// size sets req's context budget in estimated tokens from budget, the
// caller's budget in provider tokens (0 for none): the budget over the
// conversation's ratio, within what a stated window leaves the input after
// the output reserve, and within any limit a rejection set. The reserve
// gives way, lowering req.MaxTokens, only as far as leaving the input half
// the window, or the budget when that is less.
func (c *Calibration) size(req *CompletionRequest, budget int) {
	c.mu.Lock()
	m := c.models[calibrationRoute(req)]
	ratio := c.scale(calibrationScope(req))
	c.mu.Unlock()
	limit := budget
	if m.window > 0 {
		reserve := req.MaxTokens
		if reserve <= 0 {
			reserve = m.output
		}
		keep := m.window / 2
		if budget > 0 {
			keep = min(keep, budget)
		}
		if reserve > m.window-keep {
			reserve = m.window - keep
			req.MaxTokens = reserve
		}
		limit = m.window - reserve
		if budget > 0 {
			limit = min(limit, budget)
		}
	}
	if m.limit > 0 && (limit <= 0 || m.limit < limit) {
		limit = m.limit
	}
	if limit > 0 {
		limit = max(1, int(float64(limit)/ratio))
	}
	req.MaxContextTokens = limit
}
