package coordindex

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestLifecycleSelectorRelations(t *testing.T) {
	cases := []struct {
		name, kind, id, payload string
		want, absent            []Relation
	}{
		{"execution", "execution", "e", `{"run":"R/\u0000☃","request":{"callID":"c:'[]"},"callID":"not-nested"}`, []Relation{{ExecutionRunCall, Key("R/\x00☃", "c:'[]")}}, []Relation{{ExecutionRunCall, Key("R/\x00☃", "not-nested")}}},
		{"missing request", "execution", "e", `{"run":"r"}`, []Relation{{ExecutionRunCall, Key("r", "")}}, nil},
		{"null request", "execution", "e", `{"request":null}`, []Relation{{ExecutionRunCall, Key("", "")}}, nil},
		{"null call", "execution", "e", `{"request":{"callID":null}}`, []Relation{{ExecutionRunCall, Key("", "")}}, nil},
		{"exact field", "execution", "e", `{"run":"r","Request":{"callID":"outer"},"request":{"CallID":"inner"}}`, []Relation{{ExecutionRunCall, Key("r", "")}}, []Relation{{ExecutionRunCall, Key("r", "outer")}, {ExecutionRunCall, Key("r", "inner")}}},
		{"start", "mail", "m", `{"from":"a/b","callID":"C\u0000[]","to":"member","start":true}`, []Relation{{MailFromCall, Key("a/b", "C\x00[]")}, {MailPendingStartRecipient, "member"}}, nil},
		{"ordinary", "mail", "m", `{"to":"member"}`, []Relation{{MailFromCall, Key("", "")}}, []Relation{{MailPendingStartRecipient, "member"}}},
		{"delivered", "mail", "m", `{"to":"member","start":true,"delivered":true}`, nil, []Relation{{MailPendingStartRecipient, "member"}}},
		{"false start", "mail", "m", `{"to":"member","start":false}`, nil, []Relation{{MailPendingStartRecipient, "member"}}},
		{"null start", "mail", "m", `{"to":"member","start":null}`, nil, []Relation{{MailPendingStartRecipient, "member"}}},
		{"read not delivery", "mail", "m", `{"to":"member","start":true,"read":true}`, []Relation{{MailPendingStartRecipient, "member"}}, nil},
		{"canonical name", "member", "m", `{"agentName":"/root/Team/Child"}`, []Relation{{MemberName, "/root/Team/Child"}}, []Relation{{MemberName, "/root/Team"}, {MemberName, "/root/team/child"}, {MemberName, "/root/Team/Child/"}}},
		{"name exact", "member", "m", `{"agentName":"/root/odd/ "}`, []Relation{{MemberName, "/root/odd/ "}}, []Relation{{MemberName, "/root/odd"}}},
		{"fallback payload id", "member", "key", `{"id":"payload"}`, []Relation{{MemberName, "/root/agent_payload"}}, []Relation{{MemberName, "/root/agent_key"}}},
		{"fallback record id", "member", "key", `{}`, []Relation{{MemberName, "/root/agent_key"}}, nil},
		{"name authority", "member", "key", `{"id":"payload","agentName":"/root/named"}`, []Relation{{MemberName, "/root/named"}}, []Relation{{MemberName, "/root/agent_payload"}}},
		{"task followup", "task", "t", `{"follows":"a/b","followupCallID":"c:'[]"}`, []Relation{{TaskFollowupCall, Key("a/b", "c:'[]")}}, nil},
		{"task empty", "task", "t", `{}`, []Relation{{TaskFollowupCall, Key("", "")}}, nil},
		{"snapshot source", "snapshot", "s", `{"source":"/a//b/../☃\u0000"}`, []Relation{{SnapshotSource, "/a//b/../☃\x00"}}, []Relation{{SnapshotSource, "/a/☃\x00"}}},
		{"snapshot empty", "snapshot", "s", `{}`, []Relation{{SnapshotSource, ""}}, nil},
		{"call ID", "execution", "e", `{"request":{"callID":"c:'[]"}}`, []Relation{{ExecutionCallID, "c:'[]"}}, nil},
		{"empty call ID", "execution", "e", `{}`, []Relation{{ExecutionCallID, ""}}, nil},
		{"step ID", "workflow_step", "w/10", `{"id":"stable/☃\u0000"}`, []Relation{{WorkflowStepID, Key("w", "stable/☃\x00")}}, []Relation{{WorkflowStepID, Key("w", "10")}}},
		{"empty step ID", "workflow_step", "w/2", `{}`, []Relation{{WorkflowStepID, Key("w", "")}}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := Extract(test.kind, test.id, json.RawMessage(test.payload))
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
					t.Fatalf("unexpected %+v in %+v", absent, got)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].Index > got[i].Index || got[i-1].Index == got[i].Index && got[i-1].Key >= got[i].Key {
					t.Fatalf("unsorted or duplicate relations: %+v", got)
				}
			}
		})
	}
}

func TestLifecycleSelectorMalformedFields(t *testing.T) {
	for _, test := range []struct{ kind, payload string }{
		{"execution", `{"request":[]}`}, {"execution", `{"request":"bad"}`}, {"execution", `{"request":1}`},
		{"execution", `{"request":{"callID":[]}}`}, {"execution", `{"request":{"callID":{}}}`}, {"execution", `{"request":{"callID":1}}`},
		{"mail", `{"callID":[]}`}, {"mail", `{"callID":1}`}, {"mail", `{"start":"true"}`}, {"mail", `{"start":1}`},
		{"mail", `{"start":[]}`}, {"mail", `{"start":true,"delivered":{}}`}, {"member", `{"agentName":[]}`},
		{"task", `{"follows":[]}`}, {"task", `{"followupCallID":{}}`}, {"snapshot", `{"source":1}`},
		{"workflow_step", `{"id":[]}`},
	} {
		id := "id"
		if test.kind == "workflow_step" {
			id = "w/0"
		}
		relations, _, err := Extract(test.kind, id, json.RawMessage(test.payload))
		if !errors.Is(err, ErrFormat) || len(relations) != 0 {
			t.Fatalf("accepted %s %s: %+v %v", test.kind, test.payload, relations, err)
		}
	}
}

func TestLifecycleSelectorKeys(t *testing.T) {
	for _, index := range []Index{ExecutionRunCall, MailFromCall, TaskFollowupCall, WorkflowStepID} {
		seen := map[string]bool{}
		for _, parts := range [][]string{{"a", "bc"}, {"ab", "c"}, {"a/b", "c"}, {"a", "b/c"}, {"", ""}, {"\xff\x00'[]", " /☃: "}} {
			key := Key(parts...)
			gotIndex, gotKey, err := Normalize(index, key)
			if err != nil || gotIndex != index || gotKey != key || seen[key] {
				t.Fatalf("%s %q: %s %q %v (collision=%t)", index, parts, gotIndex, gotKey, err, seen[key])
			}
			seen[key] = true
		}
		for _, key := range []string{"a/b", "!", Key(), Key("one"), Key("a", "b", "c")} {
			if _, _, err := Normalize(index, key); !errors.Is(err, ErrSelector) {
				t.Fatalf("accepted %s key %q: %v", index, key, err)
			}
		}
	}
	for _, index := range []Index{MemberName, MailPendingStartRecipient, SnapshotSource, ExecutionCallID} {
		for _, key := range []string{"", "/root/a", "/root/a/", "/root/A", "/root/a/b", "\x00\xff'[] ☃"} {
			gotIndex, gotKey, err := Normalize(index, key)
			if err != nil || gotIndex != index || gotKey != key {
				t.Fatalf("normalized exact selector %s %q: %s %q %v", index, key, gotIndex, gotKey, err)
			}
		}
	}
}
