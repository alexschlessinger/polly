package docker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// mirror is the host registry of proxies for the tools one helper session
// serves. It applies the scope's selection on this side as well, so tools a
// skill stages later are bounded by it, and the binding's Close ends the
// session before releasing the registry. A copy-mode binding hooks every
// call to synchronise the workspace.
type mirror struct {
	session  *session
	registry *tools.ToolRegistry
	mu       sync.Mutex
	once     sync.Once
	// inflight counts calls being executed, for resync's refusal.
	inflight atomic.Int32
	// before runs ahead of a call; after runs once it finished, with its
	// outcome, and may replace the call's error.
	before func(context.Context) error
	after  func(ctx context.Context, info protocol.ToolInfo, result protocol.Result, err error) error
}

// bindSession builds the ToolBinding for a helper that answered load.
func bindSession(scope tools.ToolScope, s *session, loaded protocol.Loaded) (*mirror, tools.ToolBinding) {
	m := &mirror{session: s, registry: tools.NewToolRegistry(nil)}
	for _, info := range loaded.Tools {
		m.add(info, false)
	}
	m.registry.RestrictView(scope.AllowedTools)
	return m, tools.ToolBinding{
		Registry:         m.registry,
		Instructions:     loaded.Instructions,
		ToolInstructions: loaded.ToolInstructions,
		Omitted:          append([]string(nil), loaded.Omitted...),
		Close:            m.close,
	}
}

func (m *mirror) add(info protocol.ToolInfo, staged bool) {
	proxy := newProxy(m, info)
	if staged {
		m.registry.StageTools(proxy)
	} else {
		m.registry.Register(proxy)
	}
	if info.AlwaysAllowed {
		m.registry.MarkAlwaysAllowed(info.Name)
	}
	if info.Builtin {
		m.registry.MarkBuiltin(info.Name)
	}
}

// stage mirrors tools a skill activation registered helper-side and the
// allowance it declared, for the next commit on this registry.
func (m *mirror) stage(infos []protocol.ToolInfo, allowance *protocol.Allowance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, info := range infos {
		m.add(info, true)
	}
	if allowance != nil {
		m.registry.StageSkillAllowance(allowance.Patterns, allowance.AutoAllowed)
	}
}

func (m *mirror) close() error {
	var err error
	m.once.Do(func() {
		err = errors.Join(m.session.Close(), m.registry.Close())
	})
	return err
}
