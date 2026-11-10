package dsh

import "testing"

// testValidHost 返回一个通过版本门禁的发现型宿主，供各测试按需修改。
func testValidHost() Host {
	return Host{
		PID:        4242,
		Port:       3080,
		Version:    "0.2.1-rc.5",
		Variant:    "web",
		InstallDir: "/Applications/DeepSeek Harness.app",
		Home:       "/Users/alice/.dsh",
		Executable: "/usr/local/bin/dsh",
		Generation: "0123456789abcdef01234567",
	}
}

func TestHostReferenceIncludesGeneration(t *testing.T) {
	host := testValidHost()
	want := "harness:4242:3080:0123456789abcdef01234567"
	if got := host.Reference(); got != want {
		t.Fatalf("Reference() = %q, want %q", got, want)
	}

	restarted := host
	restarted.Generation = "fedcba9876543210fedcba98"
	if restarted.Reference() == host.Reference() {
		t.Fatal("Generation 变化后宿主引用必须变化")
	}
}

func TestHostURLIsLoopbackRoot(t *testing.T) {
	host := testValidHost()
	if got, want := host.URL(), "http://127.0.0.1:3080/"; got != want {
		t.Fatalf("URL() = %q, want %q", got, want)
	}
}

func TestHostValidateAcceptsCheckedVersions(t *testing.T) {
	tests := []struct {
		name string
		host Host
	}{
		{
			name: "0.2.1-rc.5 web",
			host: func() Host {
				host := testValidHost()
				host.Port = 1
				return host
			}(),
		},
		{
			name: "0.2.2-rc.1 desktop",
			host: func() Host {
				host := testValidHost()
				host.Version = "0.2.2-rc.1"
				host.Variant = "desktop"
				host.Port = 65535
				return host
			}(),
		},
		{
			name: "explicit without pid",
			host: func() Host {
				host := testValidHost()
				host.PID = 0
				host.Version = "explicit"
				return host
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.host.Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestHostValidateRejectsUnknownVersionAndBadPort(t *testing.T) {
	tests := []struct {
		name string
		host Host
	}{
		{
			name: "unknown version",
			host: func() Host {
				host := testValidHost()
				host.Version = "0.2.3-rc.1"
				return host
			}(),
		},
		{
			name: "empty version",
			host: func() Host {
				host := testValidHost()
				host.Version = ""
				return host
			}(),
		},
		{
			name: "port zero",
			host: func() Host {
				host := testValidHost()
				host.Port = 0
				return host
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.host.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestHostValidateRejectsUnknownVariant(t *testing.T) {
	tests := []struct {
		name    string
		variant string
	}{
		{name: "empty variant", variant: ""},
		{name: "unknown variant", variant: "cli"},
		{name: "case sensitive variant", variant: "Web"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := testValidHost()
			host.Variant = test.variant
			if err := host.Validate(); err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}
