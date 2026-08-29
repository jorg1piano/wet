package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSystemImages(t *testing.T, root string, images ...systemImage) {
	t.Helper()
	for _, img := range images {
		dir := filepath.Join(root, "system-images", img.Platform, img.Tag, img.ABI)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source.properties"), []byte("Pkg.Revision=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A machine with images picks the newest phone image for its own ABI; a
// foreign ABI would never boot at speed and a TV image is not a phone.
func TestChooseSystemImagePrefersNewestPhoneImageForHostABI(t *testing.T) {
	root := t.TempDir()
	writeSystemImages(t, root,
		systemImage{"android-34", "google_apis", "arm64-v8a"},
		systemImage{"android-36", "google_apis_playstore_ps16k", "arm64-v8a"},
		systemImage{"android-36", "android-tv", "arm64-v8a"},
		systemImage{"android-37", "google_apis", "x86_64"},
		systemImage{"android-Baklava", "google_apis", "arm64-v8a"},
	)
	// A directory with no source.properties is a half-installed package.
	if err := os.MkdirAll(filepath.Join(root, "system-images", "android-38", "google_apis", "arm64-v8a"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := chooseSystemImage(installedSystemImages(root), "arm64-v8a")
	if !ok {
		t.Fatal("expected an image")
	}
	if want := "system-images;android-36;google_apis_playstore_ps16k;arm64-v8a"; got.Package() != want {
		t.Errorf("chose %s, want %s", got.Package(), want)
	}
}

// Within one API level the plain Google APIs image wins over the Play Store
// and AOSP flavours.
func TestChooseSystemImageBreaksTiesByTag(t *testing.T) {
	images := []systemImage{
		{"android-35", "google_apis_playstore", "x86_64"},
		{"android-35", "default", "x86_64"},
		{"android-35", "google_apis", "x86_64"},
	}
	got, ok := chooseSystemImage(images, "x86_64")
	if !ok || got.Tag != "google_apis" {
		t.Errorf("chose %+v, want google_apis", got)
	}
}

func TestChooseSystemImageReportsNoneForHostABI(t *testing.T) {
	images := []systemImage{{"android-36", "google_apis", "x86_64"}}
	if _, ok := chooseSystemImage(images, "arm64-v8a"); ok {
		t.Error("an x86_64-only SDK must not satisfy an arm64 host")
	}
}

func TestAndroidSDKRootHonoursEnvironment(t *testing.T) {
	t.Setenv("ANDROID_HOME", "/opt/sdk")
	t.Setenv("ANDROID_SDK_ROOT", "/opt/other")
	if root, err := androidSDKRoot(); err != nil || root != "/opt/sdk" {
		t.Errorf("root = %q, %v; want ANDROID_HOME", root, err)
	}
	t.Setenv("ANDROID_HOME", "")
	if root, err := androidSDKRoot(); err != nil || root != "/opt/other" {
		t.Errorf("root = %q, %v; want ANDROID_SDK_ROOT", root, err)
	}
}
