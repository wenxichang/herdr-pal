package installer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/config"
)

const validMachineKey = "hpk_2_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestMergeClientConfigPreservesNonRelaySettings(t *testing.T) {
	existing := []byte(`{
  "relay":{"url":"wss://old.example/hprp","key":"hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","skip_verify":false},
  "herdr":{"session":"work","socket_path":"/tmp/custom.sock"},
  "log":{"level":"debug"}
}`)

	merged, err := mergeClientConfig(existing, "wss://new.example/hprp", validMachineKey)
	if err != nil {
		t.Fatal(err)
	}
	var got config.ClientConfig
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Relay.URL != "wss://new.example/hprp" || got.Relay.Key != validMachineKey || got.Relay.SkipVerify {
		t.Fatalf("relay = %+v", got.Relay)
	}
	if got.Herdr.Session != "work" || got.Herdr.SocketPath != "/tmp/custom.sock" || got.Log.Level != "debug" {
		t.Fatalf("non-relay settings changed: %+v", got)
	}
}

func TestMergeClientConfigCreatesSafeDefaults(t *testing.T) {
	merged, err := mergeClientConfig(nil, "wss://relay.example/hprp", validMachineKey)
	if err != nil {
		t.Fatal(err)
	}
	var got config.ClientConfig
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Relay.URL != "wss://relay.example/hprp" || got.Relay.Key != validMachineKey || !got.Relay.SkipVerify {
		t.Fatalf("relay = %+v", got.Relay)
	}
	if got.Herdr != (config.HerdrConfig{}) || got.Log.Level != "info" {
		t.Fatalf("defaults = %+v", got)
	}
	if !strings.HasSuffix(string(merged), "\n") {
		t.Fatalf("merged config should end with newline: %q", merged)
	}
}

func TestMergeClientConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		url      string
		key      string
	}{
		{name: "invalid url", url: "http://relay.example", key: validMachineKey},
		{name: "invalid key", url: "wss://relay.example/hprp", key: "hpk_invalid"},
		{name: "multiple json values", existing: `{}` + "\n{}", url: "wss://relay.example/hprp", key: validMachineKey},
		{name: "invalid relay object", existing: `{"relay":[]}`, url: "wss://relay.example/hprp", key: validMachineKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := mergeClientConfig([]byte(test.existing), test.url, test.key); err == nil {
				t.Fatal("mergeClientConfig() should fail")
			}
		})
	}
}
