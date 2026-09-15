// Package helper is the polly process inside a container: it hosts a real
// native tool registry bound to the container's worktree and serves it to
// the host over the protocol on its stdin and stdout. The host's registry
// holds one proxy per tool served here; approval, timeouts, gates and result
// handling stay on the host, and the same tool code runs against the
// container's filesystem.
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Options configures Serve.
type Options struct {
	// Version identifies the helper build in the welcome; informational.
	Version string
	// Instructions loads repository guidance for the bound registry.
	Instructions func(*tools.ToolRegistry) string
	// Factory builds the sandbox factory from the sealed environment. The
	// default is sandbox.NewContainerSandboxFactory; tests substitute one.
	Factory func(sealed []string) (func(sandbox.Config) (sandbox.Sandbox, error), error)
	// ExtraTools are registered on the source registry before binding, for
	// tests that need a tool the loaders cannot produce.
	ExtraTools []tools.Tool
	// Home overrides the home directory the hello names, for tests that
	// run the helper outside a container.
	Home string
}

// ErrProtocolMismatch reports a host speaking another protocol version.
var ErrProtocolMismatch = errors.New("helper protocol mismatch")

type server struct {
	opts Options
	conn *protocol.Conn

	mu       sync.Mutex
	hello    *protocol.Hello
	factory  func(sandbox.Config) (sandbox.Sandbox, error)
	base     sandbox.Config
	source   *tools.ToolRegistry
	catalog  *skills.Catalog
	binding  tools.ToolBinding
	loaded   bool
	copy     *copyState
	inflight map[uint64]context.CancelFunc
	wg       sync.WaitGroup

	// activateMu serialises skill activations so each result's staged tools
	// are attributable to one activation.
	activateMu sync.Mutex
}

// Serve answers requests on stdin until it ends, then cancels in-flight
// executions and releases the registry. It returns ErrProtocolMismatch when
// the host's hello names another version, and a transport error otherwise.
func Serve(ctx context.Context, stdin io.Reader, stdout io.Writer, opts Options) error {
	if opts.Factory == nil {
		opts.Factory = sandbox.NewContainerSandboxFactory
	}
	s := &server{opts: opts, conn: protocol.NewConn(stdin, stdout), inflight: map[uint64]context.CancelFunc{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.shutdown()
	for {
		frame, err := s.conn.Read()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if errors.Is(err, protocol.ErrFrameTooLarge) {
			_ = s.conn.Send(0, protocol.TypeError, protocol.Error{Code: protocol.CodeFrame, Message: err.Error()})
			return err
		}
		if err != nil {
			return err
		}
		if err := s.dispatch(ctx, frame); err != nil {
			return err
		}
	}
}

func (s *server) shutdown() {
	s.mu.Lock()
	for id, cancel := range s.inflight {
		cancel()
		delete(s.inflight, id)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding.Close != nil {
		_ = s.binding.Close()
	}
	if s.source != nil {
		_ = s.source.Close()
	}
}

func (s *server) fail(id uint64, code, message string) error {
	return s.conn.Send(id, protocol.TypeError, protocol.Error{Code: code, Message: message})
}

func (s *server) dispatch(ctx context.Context, frame protocol.Frame) error {
	switch frame.Type {
	case protocol.TypeHello:
		return s.handleHello(frame)
	case protocol.TypeLoad:
		return s.handleLoad(ctx, frame)
	case protocol.TypeList:
		s.mu.Lock()
		loaded := s.loaded
		var registry *tools.ToolRegistry
		if loaded {
			registry = s.binding.Registry
		}
		s.mu.Unlock()
		if !loaded {
			return s.fail(frame.ID, protocol.CodeNotLoaded, "tools are not loaded")
		}
		return s.conn.Send(frame.ID, protocol.TypeListed, protocol.Listed{Tools: describeAll(registry)})
	case protocol.TypeExecute:
		return s.handleExecute(ctx, frame)
	case protocol.TypeCancel:
		req, err := protocol.Decode[protocol.Cancel](frame)
		if err != nil {
			return s.fail(frame.ID, protocol.CodeProtocol, err.Error())
		}
		s.mu.Lock()
		if cancel := s.inflight[req.ID]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
		return s.conn.Send(frame.ID, protocol.TypeOK, nil)
	case protocol.TypeHeartbeat:
		return s.conn.Send(frame.ID, protocol.TypePong, nil)
	case protocol.TypeSync:
		return s.handleSync(frame)
	default:
		return s.fail(frame.ID, protocol.CodeProtocol, fmt.Sprintf("unknown request %q", frame.Type))
	}
}

func (s *server) handleHello(frame protocol.Frame) error {
	hello, err := protocol.Decode[protocol.Hello](frame)
	if err != nil {
		return s.fail(frame.ID, protocol.CodeProtocol, err.Error())
	}
	if hello.Protocol != protocol.Version {
		_ = s.fail(frame.ID, protocol.CodeProtocol, fmt.Sprintf("helper speaks protocol %d, host requires %d", protocol.Version, hello.Protocol))
		return ErrProtocolMismatch
	}
	s.mu.Lock()
	already := s.hello != nil
	s.mu.Unlock()
	if already {
		return s.fail(frame.ID, protocol.CodeProtocol, "hello already received")
	}
	if hello.Root == "" || !filepath.IsAbs(hello.Root) {
		return s.fail(frame.ID, protocol.CodeProtocol, "hello names no absolute root")
	}
	if hello.Scratch != "" {
		// A copy's scratch inside the container's private temp is created
		// here; a mounted scratch already exists.
		if err := os.MkdirAll(hello.Scratch, 0o700); err != nil {
			return s.fail(frame.ID, protocol.CodeInternal, fmt.Sprintf("create scratch: %v", err))
		}
	}
	home := hello.Home
	if s.opts.Home != "" {
		home = s.opts.Home
	}
	if home != "" {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return s.fail(frame.ID, protocol.CodeInternal, fmt.Sprintf("create home: %v", err))
		}
		if err := writeGitIdentity(home, hello.GitIdent); err != nil {
			return s.fail(frame.ID, protocol.CodeInternal, fmt.Sprintf("write git identity: %v", err))
		}
	}
	factory, err := s.opts.Factory(hello.Env)
	if err != nil {
		_ = s.fail(frame.ID, protocol.CodeInternal, fmt.Sprintf("sandbox factory: %v", err))
		return err
	}
	base := sandbox.Config{
		WritablePaths:  []string{hello.Root},
		AllowNetwork:   hello.Network.Allow,
		DenyDNS:        hello.Network.DenyDNS,
		AllowEnv:       append([]string(nil), hello.AllowEnv...),
		PassEnv:        append([]string(nil), hello.PassEnv...),
		DenyPaths:      existingPaths(hello.DeniedReads),
		DenyWritePaths: existingPaths(hello.DeniedWrites),
	}
	if hello.Scratch != "" {
		base.WritablePaths = append(base.WritablePaths, hello.Scratch)
	}
	s.mu.Lock()
	s.hello = &hello
	s.factory = factory
	s.base = base
	s.mu.Unlock()
	return s.conn.Send(frame.ID, protocol.TypeWelcome, protocol.Welcome{
		Protocol: protocol.Version, Polly: s.opts.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH, UID: os.Getuid(), GID: os.Getgid(),
	})
}

// writeGitIdentity writes the identity from values into the helper's own
// Git configuration; nothing of the host's configuration travels.
func writeGitIdentity(home string, ident protocol.GitIdentity) error {
	if ident.Name == "" && ident.Email == "" {
		return nil
	}
	content := "[user]\n"
	if ident.Name != "" {
		content += "\tname = " + ident.Name + "\n"
	}
	if ident.Email != "" {
		content += "\temail = " + ident.Email + "\n"
	}
	content += "[commit]\n\tgpgsign = false\n"
	return os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(content), 0o600)
}

// existingPaths keeps the paths that exist inside the container: a host
// path that is not mounted here needs no rule, and a missing rule would
// fail preparation.
func existingPaths(paths []string) []string {
	var kept []string
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			kept = append(kept, path)
		}
	}
	return kept
}

func (s *server) handleLoad(ctx context.Context, frame protocol.Frame) error {
	load, err := protocol.Decode[protocol.Load](frame)
	if err != nil {
		return s.fail(frame.ID, protocol.CodeProtocol, err.Error())
	}
	s.mu.Lock()
	hello, factory, base, loaded := s.hello, s.factory, s.base, s.loaded
	s.mu.Unlock()
	if hello == nil {
		return s.fail(frame.ID, protocol.CodeProtocol, "load before hello")
	}
	if loaded {
		return s.fail(frame.ID, protocol.CodeProtocol, "tools already loaded")
	}
	skillRoots := existingPaths(load.SkillRoots)
	base.ReadPaths = append(base.ReadPaths, skillRoots...)
	source := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithSandboxFactory(factory, base))
	for _, tool := range s.opts.ExtraTools {
		source.Register(tool)
	}
	omitted, warnings := loadSpecs(source, load.Tools)
	for _, spec := range load.Sources {
		if _, err := source.LoadToolAuto(spec); err != nil {
			omitted = append(omitted, spec)
			warnings = append(warnings, fmt.Sprintf("tool %s: %v", spec, err))
		}
	}

	var catalog *skills.Catalog
	if len(skillRoots) > 0 {
		catalog, err = skills.Discover(skillRoots)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skills: %v", err))
			catalog = nil
		}
	}
	if catalog != nil && !catalog.IsEmpty() {
		runtime, err := tools.NewSkillRuntime(catalog, source)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skills: %v", err))
		} else {
			if err := runtime.Restore(load.ActiveSkills); err != nil {
				warnings = append(warnings, fmt.Sprintf("restore skills: %v", err))
			}
			for _, name := range load.AutoActivate {
				if _, err := runtime.Activate(name); err != nil {
					warnings = append(warnings, fmt.Sprintf("activate skill %s: %v", name, err))
				}
			}
		}
	}

	var options []tools.NativeOption
	if s.opts.Instructions != nil {
		options = append(options, tools.WithNativeInstructions(s.opts.Instructions))
	}
	scope := tools.ToolScope{
		Root:       hello.Root,
		SourceRoot: hello.SourceRoot,
		Grant: tools.ExecutionGrant{
			ReadOnly:     hello.ReadOnly,
			DeniedReads:  existingPaths(hello.DeniedReads),
			DeniedWrites: existingPaths(hello.DeniedWrites),
			Scratch:      hello.Scratch,
		},
		AllowedTools: load.AllowedTools,
	}
	binding, err := tools.NativeOpenTools(source, options...)(ctx, scope)
	if err != nil {
		source.Close()
		return s.fail(frame.ID, protocol.CodeInternal, fmt.Sprintf("bind tools: %v", err))
	}
	s.mu.Lock()
	s.source, s.catalog, s.binding, s.loaded = source, catalog, binding, true
	s.mu.Unlock()
	return s.conn.Send(frame.ID, protocol.TypeLoaded, protocol.Loaded{
		Listed:           protocol.Listed{Tools: describeAll(binding.Registry)},
		Omitted:          append(omitted, binding.Omitted...),
		Instructions:     binding.Instructions,
		ToolInstructions: binding.ToolInstructions,
		Warnings:         warnings,
	})
}

// loadSpecs loads persisted tool specs the way a session restore does, but
// a source missing inside the container omits its tools instead of failing
// the whole load: paths inside the container decide what is available.
func loadSpecs(registry *tools.ToolRegistry, specs []protocol.ToolSpec) (omitted, warnings []string) {
	shell := map[string][]string{}
	mcp := map[string][]string{}
	var shellOrder, mcpOrder []string
	for _, spec := range specs {
		switch spec.Type {
		case "shell":
			if _, seen := shell[spec.Source]; !seen {
				shellOrder = append(shellOrder, spec.Source)
			}
			shell[spec.Source] = append(shell[spec.Source], spec.Name)
		case "mcp":
			if _, seen := mcp[spec.Source]; !seen {
				mcpOrder = append(mcpOrder, spec.Source)
			}
			mcp[spec.Source] = append(mcp[spec.Source], spec.Name)
		case "native":
			if _, err := registry.LoadToolAuto(spec.Name); err != nil {
				omitted = append(omitted, spec.Name)
				if registry.HasNativeTool(spec.Name) {
					warnings = append(warnings, fmt.Sprintf("native tool %s: %v", spec.Name, err))
				}
			}
		default:
			omitted = append(omitted, spec.Name)
			warnings = append(warnings, fmt.Sprintf("tool %s: unknown type %q", spec.Name, spec.Type))
		}
	}
	for _, path := range shellOrder {
		if _, err := registry.LoadShellTool(path); err != nil {
			omitted = append(omitted, shell[path]...)
			warnings = append(warnings, fmt.Sprintf("shell tool %s: %v", path, err))
		}
	}
	for _, server := range mcpOrder {
		if err := registry.LoadMCPServerWithFilter(server, mcp[server]); err != nil {
			omitted = append(omitted, mcp[server]...)
			warnings = append(warnings, fmt.Sprintf("MCP server %s: %v", server, err))
		}
	}
	return omitted, warnings
}

func describeAll(registry *tools.ToolRegistry) []protocol.ToolInfo {
	all := registry.All()
	infos := make([]protocol.ToolInfo, 0, len(all))
	for _, tool := range all {
		infos = append(infos, describe(registry, tool))
	}
	return infos
}

// describe reports a tool's schema and the optional interfaces it presents,
// so the host proxy can present the same ones.
func describe(registry *tools.ToolRegistry, tool tools.Tool) protocol.ToolInfo {
	info := protocol.ToolInfo{Name: tool.GetName(), Type: tool.GetType(), Source: tool.GetSource()}
	if s := tool.GetSchema(); s != nil {
		if raw, err := json.Marshal(s.Raw); err == nil {
			info.Schema = raw
		}
		info.Strict = s.Strict
	}
	_, info.Output = tool.(tools.OutputTool)
	if untimed, ok := tool.(tools.UntimedTool); ok {
		info.Untimed = untimed.Untimed()
	}
	if exclusive, ok := tool.(tools.ExclusiveTool); ok {
		info.Exclusive = exclusive.ExclusiveBatch()
	}
	if coordinates, ok := tool.(tools.CoordinationTool); ok {
		info.Coordinates = coordinates.Coordinates()
	}
	if stub, ok := tools.RecallStub(tool); ok {
		info.RecallStub = stub
	}
	info.AlwaysAllowed = registry.AlwaysAllowed(info.Name)
	info.Builtin = registry.Builtin(info.Name)
	if details := tools.SandboxDetails(tool); details.Capable {
		mirror := &protocol.SandboxInfo{Capable: true, Active: details.Active, OptedOut: details.OptedOut}
		if details.Config != nil {
			mirror.WritablePaths = append([]string(nil), details.Config.WritablePaths...)
		}
		info.Sandbox = mirror
	}
	return info
}

func (s *server) handleExecute(ctx context.Context, frame protocol.Frame) error {
	req, err := protocol.Decode[protocol.Execute](frame)
	if err != nil {
		return s.fail(frame.ID, protocol.CodeProtocol, err.Error())
	}
	s.mu.Lock()
	loaded := s.loaded
	var registry *tools.ToolRegistry
	if loaded {
		registry = s.binding.Registry
	}
	s.mu.Unlock()
	if !loaded {
		return s.fail(frame.ID, protocol.CodeNotLoaded, "tools are not loaded")
	}
	tool, ok := registry.Get(req.Tool)
	if !ok {
		return s.fail(frame.ID, protocol.CodeNotFound, fmt.Sprintf("tool %q is unavailable", req.Tool))
	}
	execCtx, cancel := context.WithCancel(ctx)
	if req.TimeoutMillis > 0 {
		execCtx, cancel = context.WithTimeout(execCtx, time.Duration(req.TimeoutMillis)*time.Millisecond)
	}
	if req.Pipefail {
		execCtx = tools.WithPipefail(execCtx)
	}
	s.mu.Lock()
	s.inflight[frame.ID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, frame.ID)
			s.mu.Unlock()
			cancel()
		}()
		result := s.run(execCtx, registry, tool, req)
		_ = s.conn.Send(frame.ID, protocol.TypeResult, result)
	}()
	return nil
}

// run executes one tool and encodes its outcome. A skill activation is
// serialised and committed here, and the tools it registered are reported
// for the host to mirror: the helper's commit is unobservable until the host
// routes to them, so batch atomicity is the host's commit.
func (s *server) run(ctx context.Context, registry *tools.ToolRegistry, tool tools.Tool, req protocol.Execute) protocol.Result {
	activation := req.Tool == "activate_skill"
	var before []string
	if activation {
		s.activateMu.Lock()
		defer s.activateMu.Unlock()
		before = names(registry.All())
	}
	execution, err := registry.ExecuteTool(ctx, tool, req.Args, 0)
	result := protocol.Result{Invoked: execution.Invoked, Text: execution.Output.Text}
	if execution.Output.Data != nil {
		if encoded, marshalErr := json.Marshal(execution.Output.Data); marshalErr == nil {
			result.Data = encoded
		}
	}
	for _, media := range execution.Output.Media {
		result.Media = append(result.Media, protocol.Media{Data: media.Data, MIMEType: media.MIMEType, Name: media.Name, Reference: media.Reference})
	}
	if err != nil {
		result.Error = encodeError(err)
	}
	switch {
	case errors.Is(execution.ContextErr, context.DeadlineExceeded):
		result.ContextErr = protocol.ContextDeadline
	case errors.Is(execution.ContextErr, context.Canceled):
		result.ContextErr = protocol.ContextCanceled
	}
	if err == nil && (req.Tool == "write_file" || req.Tool == "edit_file") {
		result.Wrote = true
	}
	if activation && execution.Invoked {
		registry.CommitPendingChanges()
		after := names(registry.All())
		for _, tool := range registry.All() {
			if !slices.Contains(before, tool.GetName()) {
				result.Staged = append(result.Staged, describe(registry, tool))
			}
		}
		if err == nil {
			result.Allowance = s.allowance(req.Args, after, before)
		}
	}
	return result
}

// allowance reports the allow-list a successful activation staged: the
// skill's declared patterns and the tools it loaded.
func (s *server) allowance(args map[string]any, after, before []string) *protocol.Allowance {
	name, _ := args["name"].(string)
	s.mu.Lock()
	catalog := s.catalog
	s.mu.Unlock()
	allowance := &protocol.Allowance{}
	if catalog != nil {
		if skill, ok := catalog.Get(name); ok {
			allowance.Patterns = tools.ParseAllowedToolPatterns(skill.AllowedTools)
		}
	}
	for _, loaded := range after {
		if !slices.Contains(before, loaded) {
			allowance.AutoAllowed = append(allowance.AutoAllowed, loaded)
		}
	}
	if len(allowance.Patterns) == 0 && len(allowance.AutoAllowed) == 0 {
		return nil
	}
	return allowance
}

func encodeError(err error) *protocol.ToolError {
	var toolErr *tools.ToolError
	if errors.As(err, &toolErr) {
		return &protocol.ToolError{Kind: protocol.ErrorKindTool, Message: toolErr.Message, Code: toolErr.Code}
	}
	var commandErr *tools.CommandError
	if errors.As(err, &commandErr) {
		return &protocol.ToolError{Kind: protocol.ErrorKindCommand, Message: err.Error(), ExitCode: commandErr.ExitCode}
	}
	return &protocol.ToolError{Kind: protocol.ErrorKindPlain, Message: err.Error()}
}

func names(all []tools.Tool) []string {
	out := make([]string, 0, len(all))
	for _, tool := range all {
		out = append(out, tool.GetName())
	}
	return out
}
