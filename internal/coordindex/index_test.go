package coordindex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"testing"
)

func TestCoordinationIndexTupleCodec(t *testing.T) {
	cases := [][]string{nil, {""}, {"", ""}, {"a", "bc"}, {"ab", "c"}, {"a/b", "c:d"}, {"\x00\xff", "☃", ""}}
	seen := map[string]bool{}
	for _, parts := range cases {
		key := Key(parts...)
		if seen[key] {
			t.Fatalf("tuple collision: %q", parts)
		}
		seen[key] = true
		got, err := SplitKey(key)
		if err != nil || !reflect.DeepEqual(got, parts) {
			t.Fatalf("tuple %q = %q %v", parts, got, err)
		}
	}
	if got := Key("a", "bc"); got != "AWECYmM" {
		t.Fatalf("codec changed: %s", got)
	}
	for _, key := range []string{"!", base64.RawURLEncoding.EncodeToString([]byte{4, 'a'}), base64.RawURLEncoding.EncodeToString([]byte{0x81, 0, 'a'})} {
		if _, err := SplitKey(key); !errors.Is(err, ErrSelector) {
			t.Fatalf("accepted malformed tuple %q: %v", key, err)
		}
	}
}

// JSON shapes mirror swarm/state.go, format.go, delegation.go and
// publication_scope.go/publication_delivery.go without importing swarm.
func TestCoordinationIndexSelectorRelations(t *testing.T) {
	cases := []struct {
		kind, id, payload string
		want              []Relation
		absent            []Relation
	}{
		{"mail", "m", `{"id":"m","to":"member","from":"sender","delivered":false,"replyTo":"q","task":"task","workflow":"w"}`, []Relation{{MailRecipient, "member"}, {MailPending, "member"}, {OverviewDependency, Key("mail", "q")}, {OverviewDependency, Key("member", "sender")}, {OverviewDependency, Key("task", "task")}, {OverviewDependency, Key("workflow", "w")}}, nil},
		{"mail", "m", `{"to":"member","delivered":true}`, []Relation{{MailRecipient, "member"}}, []Relation{{MailPending, "member"}}},
		{"member", "a", `{"id":"a","agentName":"/root/a/b","execution":"e","context":"c","task":"t","controller":"w"}`, []Relation{{MemberNamePrefix, ""}, {MemberNamePrefix, "/root"}, {MemberNamePrefix, "/root/a"}, {MemberNamePrefix, "/root/a/b"}, {OverviewDependency, Key("context", "c")}, {OverviewDependency, Key("execution", "e")}, {OverviewDependency, Key("task", "t")}, {OverviewDependency, Key("workflow", "w")}}, []Relation{{MemberNamePrefix, "/root/a/bb"}}},
		{"member", "old", `{"id":"old"}`, []Relation{{MemberNamePrefix, "/root/agent_old"}}, nil},
		{"execution", "e", `{"member":"m","run":"r","workflow":"w","status":"running","workspace":"c","base":"s"}`, []Relation{{ExecutionMember, "m"}, {ExecutionRun, "r"}, {ExecutionWorkflow, "w"}, {ExecutionStatus, "running"}, {OverviewDependency, Key("snapshot", "s")}, {OverviewDependency, Key("context", "c")}}, nil},
		{"task", "t", `{"owner":"m","execution":"e","dependencies":["a","a","b"],"status":"queued","follows":"previous","startingSnapshot":"base","snapshot":"result"}`, []Relation{{TaskOwner, "m"}, {TaskExecution, "e"}, {TaskDependency, "a"}, {TaskDependency, "b"}, {TaskStatus, "queued"}, {OverviewDependency, Key("task", "previous")}, {OverviewDependency, Key("snapshot", "base")}, {OverviewDependency, Key("snapshot", "result")}}, nil},
		{"context", "c", `{"owner":"m"}`, []Relation{{ContextOwner, "m"}, {ContextRelease, ""}}, nil},
		{"context", "c", `{"owner":"m","release":"discarded"}`, []Relation{{ContextRelease, "discarded"}}, []Relation{{ContextRelease, ""}}},
		{"workflow_step", "w/12", `{"id":"op","status":"completed"}`, []Relation{{WorkflowSteps, "w"}, {OverviewDependency, Key("workflow", "w")}}, nil},
		{"workflow", "w", `{"run":"r","steps":null}`, []Relation{{OverviewDependency, Key("run", "r")}}, nil},
		{"integration", "i", `{"inputs":[{"task":"t","base":{"id":"b"},"submitted":{"id":"s"}}],"repairs":[{"task":"fix"}],"pending":[{"task":"p"}],"successor":"next"}`, []Relation{{OverviewDependency, Key("task", "t")}, {OverviewDependency, Key("snapshot", "b")}, {OverviewDependency, Key("snapshot", "s")}, {OverviewDependency, Key("task", "fix")}, {OverviewDependency, Key("task", "p")}, {OverviewDependency, Key("integration", "next")}}, nil},
	}
	for _, test := range cases {
		t.Run(test.kind+"/"+test.id+"/"+test.payload, func(t *testing.T) {
			got, order, err := Extract(test.kind, test.id, json.RawMessage(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			if order.Kind != test.kind || order.ID != test.id {
				t.Fatalf("order %+v", order)
			}
			for _, want := range append(test.want, Relation{Records, ""}, Relation{Record, Key(test.kind, test.id)}, Relation{Kind, test.kind}) {
				if !slices.Contains(got, want) {
					t.Fatalf("missing %+v in %+v", want, got)
				}
			}
			for _, absent := range test.absent {
				if slices.Contains(got, absent) {
					t.Fatalf("unexpected %+v", absent)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].Index > got[i].Index || got[i-1].Index == got[i].Index && got[i-1].Key >= got[i].Key {
					t.Fatalf("unsorted/duplicate relations: %+v", got)
				}
			}
		})
	}
}

func TestCoordinationIndexPublicationCandidatesAndExactReceipts(t *testing.T) {
	cases := []struct {
		payload      string
		want, absent []Relation
	}{
		{`{"run":"r","key":"compiler","value":"ok"}`, []Relation{{PublicationAudience, Key("run", "r")}, {PublicationClaim, Key("run", "r", "compiler")}}, []Relation{{PublicationAudience, Key("parent")}, {PublicationAudience, Key("machine")}}},
		{`{"kind":"host","run":"old","key":"compiler","value":"ok"}`, []Relation{{PublicationAudience, Key("machine")}, {PublicationAudience, Key("parent")}, {PublicationClaim, Key("machine", "", "compiler")}}, []Relation{{PublicationAudience, Key("run", "old")}}},
		{`{"scope":"workspace","run":"r","key":"compiler"}`, []Relation{{PublicationAudience, Key("run", "r")}, {PublicationAudience, Key("parent")}, {PublicationClaim, Key("workspace", "r", "compiler")}}, nil},
		{`{"scope":"snapshot","run":"r","key":"compiler","snapshot":"s1","supersedes":"p0","duplicateOf":"canonical","admittedTo":["a","b","a"]}`, []Relation{{PublicationAudience, Key("run", "r")}, {PublicationAudience, Key("parent")}, {PublicationClaim, Key("snapshot", "r", "compiler")}, {PublicationSupersedes, "p0"}, {PublicationReceipt, "a"}, {PublicationReceipt, "b"}, {PublicationExactReceipt, Key("a", "p")}, {PublicationSnapshot, "s1"}, {OverviewDependency, Key("snapshot", "s1")}, {OverviewDependency, Key("publication", "canonical")}}, []Relation{{PublicationReceipt, "c"}}},
	}
	for _, test := range cases {
		got, _, err := Extract("publication", "p", json.RawMessage(test.payload))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range test.want {
			if !slices.Contains(got, want) {
				t.Fatalf("missing %+v in %+v", want, got)
			}
		}
		for _, absent := range test.absent {
			if slices.Contains(got, absent) {
				t.Fatalf("unexpected %+v", absent)
			}
		}
	}
	// Different snapshots with the same commit, different values and historical
	// duplicates must all survive candidate extraction. The runtime resolves them.
	for _, payload := range []string{`{"scope":"snapshot","run":"r","key":"k","snapshot":"s1","value":"yes"}`, `{"scope":"snapshot","run":"r","key":"k","snapshot":"s2","value":"no","duplicateOf":"old"}`} {
		relations, _, err := Extract("publication", "p", json.RawMessage(payload))
		if err != nil || !slices.Contains(relations, Relation{PublicationClaim, Key("snapshot", "r", "k")}) {
			t.Fatalf("candidate lost: %+v %v", relations, err)
		}
	}
}

func TestCoordinationIndexOrderingAndFormat(t *testing.T) {
	var orders []Order
	for _, row := range []struct{ kind, id, payload string }{
		{"mail", "z", `{"posted":"2026-01-01T01:00:00+01:00"}`}, {"mail", "a", `{"posted":"2026-01-01T00:00:00Z"}`}, {"mail", "b", `{"posted":"2025-01-01T00:00:00Z"}`},
		{"workflow_step", "w/10", `{}`}, {"workflow_step", "w/2", `{}`}, {"workflow_step", "w/0", `{}`},
		{"publication", "b", `{"posted":"2026-01-01T00:00:00Z"}`}, {"publication", "a", `{"posted":"2026-01-01T00:00:00Z"}`},
		{"custom", "z", `null`}, {"custom", "A", `[1]`},
	} {
		_, order, err := Extract(row.kind, row.id, json.RawMessage(row.payload))
		if err != nil {
			t.Fatal(err)
		}
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return Less(orders[i], orders[j]) })
	var got []string
	for _, order := range orders {
		got = append(got, order.Kind+":"+order.ID)
	}
	want := []string{"custom:A", "custom:z", "mail:b", "mail:a", "mail:z", "publication:a", "publication:b", "workflow_step:w/0", "workflow_step:w/2", "workflow_step:w/10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v", got)
	}
	for _, test := range []struct{ kind, id, payload string }{{"workflow_step", "w/-1", `{}`}, {"workflow_step", "w/01", `{}`}, {"workflow_step", "w/no", `{}`}, {"workflow_step", "w/1/2", `{}`}, {"workflow_step", "/1", `{}`}, {"workflow", "w", `{"steps":[{}]}`}, {"mail", "m", `{"posted":"bad"}`}, {"mail", "m", `{"delivered":"false"}`}, {"member", "m", `null`}} {
		if _, _, err := Extract(test.kind, test.id, json.RawMessage(test.payload)); !errors.Is(err, ErrFormat) {
			t.Fatalf("accepted %+v %v", test, err)
		}
	}
	for _, format := range []string{"", `{}`, `{"version":1}`, `{"version":3}`, `null`} {
		if err := CheckFormat("member", json.RawMessage(format)); !errors.Is(err, ErrFormat) {
			t.Fatalf("accepted format %s: %v", format, err)
		}
	}
	if err := CheckFormat("member", json.RawMessage(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"custom", "parent_turn", "format"} {
		if err := CheckFormat(kind, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinationIndexSelectorNormalization(t *testing.T) {
	for _, index := range []Index{Records, Record, Kind, MailRecipient, MailPending, MemberNamePrefix, ExecutionMember, ExecutionRun, ExecutionWorkflow, ExecutionStatus, TaskOwner, TaskExecution, TaskDependency, TaskStatus, ContextOwner, ContextRelease, WorkflowSteps, PublicationAudience, PublicationClaim, PublicationSupersedes, PublicationReceipt, PublicationExactReceipt, PublicationSnapshot, OverviewDependency} {
		key := "x"
		switch index {
		case Records:
			key = ""
		case Record, OverviewDependency, PublicationExactReceipt:
			key = Key("a", "b")
		case PublicationAudience:
			key = Key("run", "r")
		case PublicationClaim:
			key = Key("run", "r", "k")
		case MemberNamePrefix:
			key = "/root/x/"
		}
		got, normalized, err := Normalize(index, key)
		if err != nil || got != index {
			t.Fatalf("selector %s: %v", index, err)
		}
		if index == MemberNamePrefix && normalized != "/root/x" {
			t.Fatal(normalized)
		}
	}
	for _, test := range []Relation{{Index("unknown"), ""}, {Records, "x"}, {Record, "a/b"}, {PublicationClaim, Key("machine", "wrong", "k")}, {PublicationClaim, Key("snapshot", "r", "")}, {PublicationAudience, Key("unknown")}, {MemberNamePrefix, "/other"}, {MemberNamePrefix, "/root//a"}} {
		if _, _, err := Normalize(test.Index, test.Key); !errors.Is(err, ErrSelector) {
			t.Fatalf("accepted %+v: %v", test, err)
		}
	}
	if index, key, err := Normalize("", ""); err != nil || index != Records || key != "" {
		t.Fatalf("default %s %q %v", index, key, err)
	}
}
