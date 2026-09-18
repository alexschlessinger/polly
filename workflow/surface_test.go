package workflow

import (
	"context"
	"strings"
	"testing"
)

// An unknown member must name the namespace reached for and list what it holds.
// goja reports one as "Object has no member 'x'", which names neither, so a
// typo or an invented call is diagnosable only by rereading the API reference.
func TestUnknownMemberNamesItsNamespaceAndMembers(t *testing.T) {
	r := Runner{Host: hostFunc(func(context.Context, Operation) (any, error) { return map[string]any{"ok": true}, nil })}
	for _, c := range []struct{ body, namespace, missing, suggests string }{
		{`polly.agnet({label:"l",task:"t"})`, "polly", "agnet", "agent"},
		{`polly.researchAgent("x","y")`, "polly", "researchAgent", "research"},
		{`polly.tasks.finish({id:"x"})`, "polly.tasks", "finish", "review"},
		{`polly.integration.commit("x")`, "polly.integration", "commit", "reconcile"},
		{`polly.schema.strng()`, "polly.schema", "strng", "string"},
	} {
		_, err := r.Run(context.Background(), script("return "+c.body+";"), map[string]any{})
		if err == nil {
			t.Fatalf("%s did not fail", c.body)
		}
		message := err.Error()
		if strings.Contains(message, "Object has no member") {
			t.Fatalf("%s kept the anonymous engine message: %s", c.body, message)
		}
		for _, want := range []string{c.namespace + " has no member '" + c.missing + "'", c.suggests} {
			if !strings.Contains(message, want) {
				t.Fatalf("%s error lacks %q: %s", c.body, want, message)
			}
		}
	}
}

func TestScopeWorkObjectNamesItsUnknownMembers(t *testing.T) {
	r := Runner{Host: hostFunc(func(context.Context, Operation) (any, error) { return map[string]any{"ok": true}, nil })}
	_, err := r.Run(context.Background(), script(`return await polly.scope({label:"s"}, async w => w.agnet({task:"t"}));`), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "scope work object has no member 'agnet'") || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("scope work object error = %v", err)
	}
}

// The guard must not disturb the surface it wraps: known members, membership
// tests, spreading and awaiting all keep working, and a namespace stays
// serializable. A returned object is probed for "then" before it resolves, so
// that name in particular must stay absent rather than throw.
func TestNamedSurfacePreservesOrdinaryAccess(t *testing.T) {
	r := Runner{Host: hostFunc(func(_ context.Context, op Operation) (any, error) {
		return map[string]any{"kind": op.Kind}, nil
	})}
	for name, body := range map[string]string{
		"membership test":    `if (!("agent" in polly) || ("nope" in polly)) polly.fail("in operator broken"); return 1;`,
		"known member call":  `return await polly.agent("l","t");`,
		"nested namespace":   `return await polly.tasks.read("t");`,
		"schema builds":      `return polly.schema.object({a: polly.schema.str()});`,
		"keys are listable":  `if (Object.keys(polly).length < 10) polly.fail("keys hidden"); return 1;`,
		"scope spread":       `return await polly.scope({label:"s"}, async w => Object.keys({...w}).length);`,
		"scope await":        `return await polly.scope({label:"s"}, async w => { await w; return 1; });`,
		"scope agent":        `return await polly.scope({label:"s"}, async w => w.agent("l","t"));`,
		"parallel over work": `return await polly.parallel([1,2], async n => n*2);`,
	} {
		if _, err := r.Run(context.Background(), script(body), map[string]any{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
