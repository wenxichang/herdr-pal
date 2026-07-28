package hprp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSelectHighestCommonVersions(t *testing.T) {
	client := []string{"session.delta.v1", "session.delta.v2", "command.output.v1", "client.only.v1"}
	server := []string{"session.delta.v1", "command.output.v1", "server.only.v1"}
	want := []string{"command.output.v1", "session.delta.v1"}
	if got := SelectHighestCommonVersions(client, server); !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectHighestCommonVersions() = %#v, want %#v", got, want)
	}
}

func TestSelectHighestCommonVersionsRejectsInvalidNames(t *testing.T) {
	if got := SelectHighestCommonVersions([]string{"bad"}, []string{"bad"}); len(got) != 0 {
		t.Fatalf("SelectHighestCommonVersions() = %#v, want empty", got)
	}
}

func TestNegotiateFeaturesUsesServerEffectiveParameters(t *testing.T) {
	client := map[string]FeatureOffer{
		"terminal.inspect.v1": {Parameters: map[string]json.RawMessage{
			"max_lines":       json.RawMessage(`500`),
			"supports_paging": json.RawMessage(`true`),
			"future_option":   json.RawMessage(`{"mode":"client"}`),
		}},
		"terminal.inspect.v2": {Parameters: map[string]json.RawMessage{"max_lines": json.RawMessage(`1000`)}},
	}
	server := map[string]FeatureOffer{
		"terminal.inspect.v1": {Parameters: map[string]json.RawMessage{
			"max_lines":       json.RawMessage(`300`),
			"supports_paging": json.RawMessage(`true`),
		}},
	}
	got := NegotiateFeatures(client, server)
	if len(got) != 1 {
		t.Fatalf("NegotiateFeatures() = %#v", got)
	}
	offer, ok := got["terminal.inspect.v1"]
	if !ok || string(offer.Parameters["max_lines"]) != "300" {
		t.Fatalf("NegotiateFeatures() = %#v", got)
	}
}
