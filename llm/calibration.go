package llm

import "sync"

// Calibration keeps what providers have reported about each model route's
// requests (a model, with the host or base URL a request pins, since
// endpoints serving one model differ in window and count): how their count
// of a request's input tokens compares with the agent's estimate, and the
// context window and output reserve a rejection stated, or, when it stated
// neither, how much input it refused. An agent sizes every request by it, so
// agents that share one start from what the last request to the route
// showed, not from the estimate alone. It also keeps where each
// conversation's projection has compacted and omitted up to, so a run of the
// same conversation by another agent, such as a swarm member's next slice,
// starts from it. It is safe for concurrent use.
type Calibration struct {
	mu     sync.Mutex
	models map[string]modelCalibration
	// fronts is where each conversation's projection, by its cache session
	// id, has compacted and omitted up to (see projectionFronts).
	fronts map[string]projectionFronts
}

type modelCalibration struct {
	// ratio is the provider's count of a request's input tokens per token
	// the agent estimated for it, as last reported; zero until then.
	ratio float64
	// window is the least context window a rejection stated, and output
	// the output tokens the last rejection counted against it.
	window, output int
	// limit bounds the input, in provider tokens, below what a rejection
	// that stated no window refused.
	limit int
}

// NewCalibration returns an empty calibration; the zero value is one too.
func NewCalibration() *Calibration {
	return &Calibration{models: map[string]modelCalibration{}, fronts: map[string]projectionFronts{}}
}

// ready makes the maps the zero value lacks; c.mu is held.
func (c *Calibration) ready() {
	if c.models == nil {
		c.models = map[string]modelCalibration{}
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

// Ratio is the provider's count of model's input tokens per estimated token,
// 1 until a provider has reported one. model is a request's Model, for a
// request that pins neither a host nor a base URL.
func (c *Calibration) Ratio(model string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models[model].scale()
}

// calibrationRoute is what req's calibration is kept by: its model, and the
// host and base URL it pins, if any.
func calibrationRoute(req *CompletionRequest) string {
	if req.ModelHost == "" && req.BaseURL == "" {
		return req.Model
	}
	return req.Model + "\x00" + req.ModelHost + "\x00" + req.BaseURL
}

func (m modelCalibration) scale() float64 {
	if m.ratio == 0 {
		return 1
	}
	return m.ratio
}

// learnUsage keeps the ratio of what the provider reported a request's input
// costing to what the agent estimated it at.
func (c *Calibration) learnUsage(model string, reported, estimated int) {
	if reported <= 0 || estimated <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready()
	m := c.models[model]
	m.ratio = boundRatio(float64(reported) / float64(estimated))
	c.models[model] = m
}

// overflowStep is how far a rejection that states no count is taken to have
// missed by: the retry asks a quarter less of what the rejection left unknown.
const overflowStep = 1.25

// learnOverflow keeps what a rejection of a request estimated at estimated
// tokens, sent asking for maxTokens of output, said: the window, the output
// counted against it, and the ratio its count of the input implies. What it
// left unstated is bounded by what it refused. With the window stated but
// not the count, the provider counted more input than the window left room
// for, so the ratio rises at least that far, and a step past it. With
// neither stated, the input it counted, or would have by the ratio, was over
// the limit, so the limit falls a step below it.
func (c *Calibration) learnOverflow(model string, overflow *ContextOverflowError, estimated, maxTokens int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready()
	m := c.models[model]
	if overflow.Window > 0 && (m.window == 0 || overflow.Window < m.window) {
		m.window = overflow.Window
	}
	if overflow.Output > 0 {
		m.output = overflow.Output
	}
	if estimated <= 0 {
		c.models[model] = m
		return
	}
	counted := float64(estimated) * m.scale()
	if overflow.Input > 0 {
		counted = float64(overflow.Input)
		m.ratio = boundRatio(counted / float64(estimated))
	}
	switch {
	case overflow.Window > 0 && overflow.Input == 0:
		reserve := maxTokens
		if overflow.Output > 0 {
			reserve = overflow.Output
		}
		least := m.scale()
		if room := overflow.Window - reserve; room > 0 {
			least = max(least, float64(room)/float64(estimated))
		}
		m.ratio = boundRatio(least * overflowStep)
	case overflow.Window == 0:
		if limit := int(counted / overflowStep); m.limit == 0 || limit < m.limit {
			m.limit = max(1, limit)
		}
	}
	c.models[model] = m
}

func boundRatio(ratio float64) float64 {
	return min(maxRatio, max(minRatio, ratio))
}

// size sets req's context budget in estimated tokens from budget, the
// caller's budget in provider tokens (0 for none): the budget over the
// model's ratio, within what a stated window leaves the input after the
// output reserve, and within any limit a rejection set. The reserve gives
// way, lowering req.MaxTokens, only as far as leaving the input half the
// window, or the budget when that is less.
func (c *Calibration) size(req *CompletionRequest, budget int) {
	c.mu.Lock()
	m := c.models[calibrationRoute(req)]
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
		limit = max(1, int(float64(limit)/m.scale()))
	}
	req.MaxContextTokens = limit
}
