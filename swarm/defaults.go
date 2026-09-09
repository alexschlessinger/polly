package swarm

import (
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
)

type runtimeDefaults struct {
	request      llm.CompletionRequest
	agent        llm.AgentConfig
	instructions func(*tools.ToolRegistry) string
}

// UpdateDefaults refreshes the parent's settings for subsequent provider calls.
// Member identity, model, tools, files, and a logical execution's iteration cap
// remain fixed. The caller must not mutate shared request data afterward.
func (r *Runtime) UpdateDefaults(request llm.CompletionRequest, agent llm.AgentConfig, instructions func(*tools.ToolRegistry) string) {
	if agent.MaxIterations <= 0 {
		agent.MaxIterations = 1024
	}
	request.Messages, request.Tools, request.Skills = nil, nil, nil
	request.ResponseSchema = nil
	request.CacheSessionID, request.PromptCacheKey = "", ""
	if request.Temperature != nil {
		value := *request.Temperature
		request.Temperature = &value
	}
	if request.Stream != nil {
		value := *request.Stream
		request.Stream = &value
	}
	r.defaultsMu.Lock()
	r.defaults = &runtimeDefaults{request, agent, instructions}
	r.defaultsMu.Unlock()
}

func (r *Runtime) currentDefaults() runtimeDefaults {
	r.defaultsMu.RLock()
	defer r.defaultsMu.RUnlock()
	if r.defaults != nil {
		return *r.defaults
	}
	return runtimeDefaults{r.config.Request, r.config.Agent, r.config.Instructions}
}
