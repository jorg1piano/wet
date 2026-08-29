package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// The image a machine with no usable system image gets, and the hardware
// profile a fresh AVD takes when there is no existing AVD to clone one from.
const (
	defaultSystemImagePlatform = "android-36"
	defaultSystemImageTag      = "google_apis"
	defaultAVDDevice           = "pixel"
)

// androidSDKRoot is $ANDROID_HOME, the older $ANDROID_SDK_ROOT, or the
// directory Android Studio installs to.
func androidSDKRoot() (string, error) {
	for _, name := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if root := os.Getenv(name); root != "" {
			return root, nil
		}
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{filepath.Join(home, "Library", "Android", "sdk")}
	case "linux":
		candidates = []string{filepath.Join(home, "Android", "Sdk")}
	}
	for _, root := range candidates {
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			return root, nil
		}
	}
	return "", errors.New("Android SDK not found: set ANDROID_HOME, or install Android Studio or the SDK command-line tools")
}

// androidToolDirs is where each SDK binary lives under the SDK root.
var androidToolDirs = map[string]string{
	"sdkmanager": filepath.Join("cmdline-tools", "latest", "bin"),
	"avdmanager": filepath.Join("cmdline-tools", "latest", "bin"),
	"emulator":   "emulator",
}

// androidTool resolves an SDK binary: PATH first, then its directory under
// the SDK root, so a shell that never exported the SDK's bin directories
// still works.
func androidTool(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	root, err := androidSDKRoot()
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH, and the %w", name, err)
	}
	path := filepath.Join(root, androidToolDirs[name], name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s is neither on PATH nor at %s: install it through Android Studio's SDK Manager or `sdkmanager`", name, path)
	}
	return path, nil
}

// systemImage is one installed `system-images;<platform>;<tag>;<abi>` package.
type systemImage struct {
	Platform string // android-36
	Tag      string // google_apis
	ABI      string // arm64-v8a
}

func (s systemImage) Package() string {
	return strings.Join([]string{"system-images", s.Platform, s.Tag, s.ABI}, ";")
}

// installedSystemImages lists the system images under the SDK root. Each
// installed package carries a source.properties in its own directory.
func installedSystemImages(sdkRoot string) []systemImage {
	matches, _ := filepath.Glob(filepath.Join(sdkRoot, "system-images", "*", "*", "*", "source.properties"))
	images := make([]systemImage, 0, len(matches))
	for _, m := range matches {
		dir := filepath.Dir(m)
		images = append(images, systemImage{
			Platform: filepath.Base(filepath.Dir(filepath.Dir(dir))),
			Tag:      filepath.Base(filepath.Dir(dir)),
			ABI:      filepath.Base(dir),
		})
	}
	return images
}

// hostEmulatorABI is the one ABI the emulator runs natively on this machine.
func hostEmulatorABI() string {
	if runtime.GOARCH == "arm64" {
		return "arm64-v8a"
	}
	return "x86_64"
}

// systemImageTagRank orders the phone image flavours: plain Google APIs
// first, then AOSP, then Play Store. Anything else (the 16k-page-size
// variants, ATD images) ranks after those, and TV, Wear, Automotive and
// desktop images are not phones and never qualify.
func systemImageTagRank(tag string) (rank int, phone bool) {
	if strings.HasPrefix(tag, "android-") {
		return 0, false
	}
	switch tag {
	case "google_apis":
		return 0, true
	case "default":
		return 1, true
	case "google_apis_playstore":
		return 2, true
	}
	return 3, true
}

// apiLevel is the number in "android-36"; preview platforms named after a
// dessert have none and sort below every numbered release.
func apiLevel(platform string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(platform, "android-"))
	if err != nil {
		return 0
	}
	return n
}

// chooseSystemImage picks the phone image to build a fresh AVD on: the
// highest API level for the given ABI, ties broken by systemImageTagRank.
func chooseSystemImage(images []systemImage, abi string) (systemImage, bool) {
	var eligible []systemImage
	for _, img := range images {
		if _, phone := systemImageTagRank(img.Tag); phone && img.ABI == abi {
			eligible = append(eligible, img)
		}
	}
	if len(eligible) == 0 {
		return systemImage{}, false
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if la, lb := apiLevel(a.Platform), apiLevel(b.Platform); la != lb {
			return la > lb
		}
		ra, _ := systemImageTagRank(a.Tag)
		rb, _ := systemImageTagRank(b.Tag)
		return ra < rb
	})
	return eligible[0], true
}

// pickSystemImage names the system image a fresh AVD runs when there is no
// existing AVD to clone: the best one already installed for this machine's
// ABI, or, with none installed, the default one downloaded through sdkmanager.
func pickSystemImage() (string, error) {
	root, err := androidSDKRoot()
	if err != nil {
		return "", err
	}
	abi := hostEmulatorABI()
	if img, ok := chooseSystemImage(installedSystemImages(root), abi); ok {
		fmt.Printf("  Using installed system image %s\n", img.Package())
		return img.Package(), nil
	}
	pkg := systemImage{Platform: defaultSystemImagePlatform, Tag: defaultSystemImageTag, ABI: abi}.Package()
	fmt.Printf("  No %s system image is installed; downloading %s (this takes a few minutes)...\n", abi, pkg)
	if err := installSDKPackage(pkg); err != nil {
		return "", err
	}
	return pkg, nil
}

// installSDKPackage runs `sdkmanager --install`, answering "y" to every
// licence prompt a fresh SDK raises.
func installSDKPackage(pkg string) error {
	sdkmanager, err := androidTool("sdkmanager")
	if err != nil {
		return err
	}
	cmd := exec.Command(sdkmanager, "--install", pkg)
	cmd.Stdin = strings.NewReader(strings.Repeat("y\n", 20))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sdkmanager --install %s: %w", pkg, err)
	}
	return nil
}
