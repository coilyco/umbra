package config_test

import (
	"path/filepath"
	"testing"

	"github.com/coilyco/umbra/pkg/config"
)

// withAppDir sets the app dir for the test and restores the unset state
// after, so the package-global does not leak into sibling tests.
func withAppDir(t *testing.T, name string) {
	t.Helper()
	config.SetAppDir(name)
	t.Cleanup(func() { config.SetAppDir("") })
}

func TestAppDir_FallbackWhenUnset(t *testing.T) {
	withAppDir(t, "")
	if got := config.AppDir(); got != config.FallbackAppDir {
		t.Errorf("AppDir() = %q with no SetAppDir, want fallback %q", got, config.FallbackAppDir)
	}
}

func TestAppDir_ConsumerValueWins(t *testing.T) {
	withAppDir(t, ".ward")
	if got := config.AppDir(); got != ".ward" {
		t.Errorf("AppDir() = %q, want .ward", got)
	}
	if got := config.BaseName(); got != "ward" {
		t.Errorf("BaseName() = %q, want ward (leading dot stripped)", got)
	}
}

func TestCacheDirEnv_DerivedFromAppDir(t *testing.T) {
	cases := []struct {
		appDir string
		want   string
	}{
		{".ward", "WARD_CACHE_DIR"},
		{"", "UMBRA_CACHE_DIR"}, // fallback app dir ".umbra"
		{".my-app", "MY_APP_CACHE_DIR"},
	}
	for _, tc := range cases {
		t.Run(tc.appDir, func(t *testing.T) {
			withAppDir(t, tc.appDir)
			if got := config.CacheDirEnv(); got != tc.want {
				t.Errorf("CacheDirEnv() = %q for app dir %q, want %q", got, tc.appDir, tc.want)
			}
		})
	}
}

func TestCacheDir_EnvOverride(t *testing.T) {
	withAppDir(t, ".ward")
	override := t.TempDir()
	t.Setenv("WARD_CACHE_DIR", override)
	if got := config.CacheDir(); got != override {
		t.Errorf("CacheDir() = %q, want the WARD_CACHE_DIR override %q", got, override)
	}
}

func TestCacheDir_PerConsumerEnvIsolation(t *testing.T) {
	// ward's override must not be read through another consumer's env var,
	// and vice versa: the env name is derived per app dir.
	wardDir := t.TempDir()
	t.Setenv("WARD_CACHE_DIR", wardDir)
	t.Setenv("KIT_CACHE_DIR", t.TempDir())

	withAppDir(t, ".ward")
	if got := config.CacheDir(); got != wardDir {
		t.Errorf("ward CacheDir() = %q, want WARD_CACHE_DIR %q (read the wrong consumer's env)", got, wardDir)
	}
}

func TestCacheDir_HomeFallback(t *testing.T) {
	withAppDir(t, ".ward")
	t.Setenv("WARD_CACHE_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".ward", config.CacheSubdir)
	if got := config.CacheDir(); got != want {
		t.Errorf("CacheDir() = %q, want home fallback %q", got, want)
	}
}

func TestGlobalDir_UsesAppDir(t *testing.T) {
	withAppDir(t, ".ward")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := config.GlobalDir()
	if err != nil {
		t.Fatalf("GlobalDir: %v", err)
	}
	if want := filepath.Join(home, ".ward"); got != want {
		t.Errorf("GlobalDir() = %q, want %q (routed through app dir)", got, want)
	}
}
