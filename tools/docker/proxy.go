package docker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// proxyTool stands in for one tool the helper serves. It presents every
// optional tool interface and answers each from the helper's description:
// the registry, the loop and NamespacedTool all treat "implements but
// reports false" as "does not", so one type serves every remote tool.
type proxyTool struct {
	mirror *mirror
	info   protocol.ToolInfo
	schema *schema.ToolSchema
}

func newProxy(m *mirror, info protocol.ToolInfo) *proxyTool {
	p := &proxyTool{mirror: m, info: info, schema: schema.ToolSchemaFromJSON(info.Schema)}
	if p.schema == nil {
		p.schema = schema.Tool(info.Name, "", nil)
	}
	p.schema.Strict = info.Strict
	return p
}

func (p *proxyTool) GetName() string               { return p.info.Name }
func (p *proxyTool) GetType() string               { return p.info.Type }
func (p *proxyTool) GetSource() string             { return p.info.Source }
func (p *proxyTool) GetSchema() *schema.ToolSchema { return p.schema.Copy() }
func (p *proxyTool) Untimed() bool                 { return p.info.Untimed }
func (p *proxyTool) ExclusiveBatch() bool          { return p.info.Exclusive }
func (p *proxyTool) RecallStub() string            { return p.info.RecallStub }
func (p *proxyTool) Coordinates() bool             { return p.info.Coordinates }

// SandboxDetails reports the remote tool's sandbox posture for display.
func (p *proxyTool) SandboxDetails() tools.SandboxInfo {
	if p.info.Sandbox == nil {
		return tools.SandboxInfo{}
	}
	info := tools.SandboxInfo{Capable: p.info.Sandbox.Capable, Active: p.info.Sandbox.Active, OptedOut: p.info.Sandbox.OptedOut}
	if len(p.info.Sandbox.WritablePaths) > 0 {
		info.Config = &sandbox.Config{WritablePaths: append([]string(nil), p.info.Sandbox.WritablePaths...)}
	}
	return info
}

func (p *proxyTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	output, err := p.ExecuteOutput(ctx, args)
	return output.Text, err
}

// ExecuteOutput forwards the approved arguments, the remaining deadline and
// the pipefail marker, and rebuilds the tool's output and error on this
// side. Tools a skill activation registered helper-side are staged on the
// mirror; the agent's commit publishes them.
func (p *proxyTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	if args == nil {
		args = map[string]any{}
	}
	p.mirror.inflight.Add(1)
	defer p.mirror.inflight.Add(-1)
	if p.mirror.before != nil {
		if err := p.mirror.before(ctx); err != nil {
			return tools.ToolOutput{}, err
		}
	}
	req := protocol.Execute{Tool: p.info.Name, Args: args, Pipefail: tools.Pipefail(ctx)}
	if deadline, ok := ctx.Deadline(); ok {
		req.TimeoutMillis = max(int64(1), time.Until(deadline).Milliseconds())
	}
	result, err := p.mirror.session.execute(ctx, req)
	if err != nil {
		return tools.ToolOutput{}, err
	}
	output, err := p.decode(ctx, result)
	if p.mirror.after != nil {
		if syncErr := p.mirror.after(ctx, p.info, result, err); syncErr != nil {
			return output, syncErr
		}
	}
	return output, err
}

// decode rebuilds the tool's output and error from the helper's result.
func (p *proxyTool) decode(ctx context.Context, result protocol.Result) (tools.ToolOutput, error) {
	output := tools.ToolOutput{Text: result.Text, Data: decodeData(p.info.Name, result.Data)}
	for _, media := range result.Media {
		output.Media = append(output.Media, tools.ToolMedia{Data: media.Data, MIMEType: media.MIMEType, Name: media.Name, Reference: media.Reference})
	}
	if len(result.Staged) > 0 || result.Allowance != nil {
		p.mirror.stage(result.Staged, result.Allowance)
	}
	if !result.Invoked {
		message := "tool was not invoked"
		if result.Error != nil {
			message = result.Error.Message
		}
		return output, errors.New(message)
	}
	// A context outcome keeps its sentinel: a tool that returned its
	// context's error reports a timeout or cancellation, not a plain
	// failure with that text.
	if result.Error == nil || result.Error.Kind == protocol.ErrorKindPlain {
		switch result.ContextErr {
		case protocol.ContextDeadline:
			return output, context.DeadlineExceeded
		case protocol.ContextCanceled:
			return output, context.Canceled
		}
	}
	if result.Error != nil {
		return output, decodeError(result.Error)
	}
	return output, nil
}

// decodeData rebuilds a tool's structured data. bash's result keeps its
// concrete type because workflow steps read the exit code from it.
func decodeData(name string, raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	if name == "bash" {
		var result tools.CommandResult
		if err := json.Unmarshal(raw, &result); err == nil {
			return result
		}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func decodeError(failure *protocol.ToolError) error {
	switch failure.Kind {
	case protocol.ErrorKindTool:
		return tools.NewToolError(failure.Message, failure.Code)
	case protocol.ErrorKindCommand:
		return &tools.CommandError{ExitCode: failure.ExitCode, Cause: errors.New(failure.Message)}
	}
	return errors.New(failure.Message)
}
