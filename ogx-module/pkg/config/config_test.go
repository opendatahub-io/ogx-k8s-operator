package config

import (
	"testing"
	"testing/fstest"
)

func TestLoadFromFSReadsCamelCasePlatformVersion(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromFS(fstest.MapFS{
		"platform-name":   {Data: []byte("OpenDataHub\n")},
		"platformVersion": {Data: []byte("0.0.0\n")},
	})
	if err != nil {
		t.Fatalf("LoadFromFS() error = %v", err)
	}

	if cfg.PlatformName != "OpenDataHub" {
		t.Fatalf("PlatformName = %q, want %q", cfg.PlatformName, "OpenDataHub")
	}
	if cfg.PlatformVersion != "0.0.0" {
		t.Fatalf("PlatformVersion = %q, want %q", cfg.PlatformVersion, "0.0.0")
	}
}

func TestLoadFromFSReadsKebabCasePlatformVersion(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromFS(fstest.MapFS{
		"platform-version": {Data: []byte("1.2.3")},
	})
	if err != nil {
		t.Fatalf("LoadFromFS() error = %v", err)
	}

	if cfg.PlatformVersion != "1.2.3" {
		t.Fatalf("PlatformVersion = %q, want %q", cfg.PlatformVersion, "1.2.3")
	}
}

func TestLoadFromFSDefaultsPlatformVersionWhenMissing(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromFS(fstest.MapFS{
		"platform-name": {Data: []byte("OpenDataHub")},
	})
	if err != nil {
		t.Fatalf("LoadFromFS() error = %v", err)
	}

	if cfg.PlatformVersion != DefaultPlatformVersion {
		t.Fatalf("PlatformVersion = %q, want default %q", cfg.PlatformVersion, DefaultPlatformVersion)
	}
}

func TestCanonicalConfigKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
	}{
		{name: "platformVersion", want: KeyPlatformVersion},
		{name: "platformversion", want: KeyPlatformVersion},
		{name: "platform-version", want: KeyPlatformVersion},
		{name: "platform_version", want: KeyPlatformVersion},
		{name: "platformName", want: KeyPlatformName},
		{name: "manifests-path", want: "manifests-path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := canonicalConfigKey(tt.name); got != tt.want {
				t.Fatalf("canonicalConfigKey(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
