package herdr

import "testing"

func TestWindowsNamedPipePathMatchesHerdrGenericNamespacedMapping(t *testing.T) {
	markerPath := `C:\Users\alice\AppData\Local\herdr\herdr.sock`
	want := `\\.\pipe\C:\Users\alice\AppData\Local\herdr\herdr.sock`

	if got := windowsNamedPipePath(markerPath); got != want {
		t.Fatalf("windowsNamedPipePath() = %q, want %q", got, want)
	}
}

func TestWindowsDefaultSocketPathUsesHerdrDirectoryPriority(t *testing.T) {
	tests := []struct {
		name        string
		xdgConfig   string
		appData     string
		userProfile string
		home        string
		want        string
	}{
		{
			name:        "XDG_CONFIG_HOME 优先",
			xdgConfig:   `D:\config`,
			appData:     `C:\Users\alice\AppData\Roaming`,
			userProfile: `C:\Users\alice`,
			home:        `D:\home\alice`,
			want:        `D:\config\herdr\herdr.sock`,
		},
		{
			name:        "APPDATA 优先",
			appData:     `C:\Users\alice\AppData\Roaming`,
			userProfile: `C:\Users\alice`,
			home:        `D:\home\alice`,
			want:        `C:\Users\alice\AppData\Roaming\herdr\herdr.sock`,
		},
		{
			name:        "USERPROFILE 回退",
			userProfile: `C:\Users\alice\`,
			home:        `D:\home\alice`,
			want:        `C:\Users\alice\AppData\Roaming\herdr\herdr.sock`,
		},
		{
			name: "HOME 回退",
			home: `D:/home/alice/`,
			want: `D:\home\alice\.config\herdr\herdr.sock`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := windowsDefaultSocketPath(test.xdgConfig, test.appData, test.userProfile, test.home)
			if err != nil {
				t.Fatalf("windowsDefaultSocketPath() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("windowsDefaultSocketPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWindowsDefaultSocketPathRejectsMissingHomeDirectories(t *testing.T) {
	if _, err := windowsDefaultSocketPath("", "", "", ""); err == nil {
		t.Fatal("windowsDefaultSocketPath() should reject empty environment")
	}
}
