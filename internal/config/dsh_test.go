package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDSHValidation(t *testing.T) {
	key := testRelayKey(t)
	for _, test := range []struct {
		name, value string
		ok          bool
	}{
		{"missing backend", `{"herdr":{"disabled":true}}`, false},
		{"remote", `{"dsh":{"url":"http://example.com:123","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, false},
		{"dsh only", `{"herdr":{"disabled":true},"dsh":{"url":"http://127.0.0.1:17842","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, true},
		{"mixed", `{"dsh":{"url":"http://127.0.0.1:17842","token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, true},
		{"short token", `{"dsh":{"url":"http://127.0.0.1:17842","token":"short"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			value := `{"relay":{"url":"wss://relay.internal:9443","key":"` + key + `"},` + strings.TrimPrefix(test.value, "{")
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadClient(path); (err == nil) != test.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
