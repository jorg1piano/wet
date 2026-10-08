package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBootingADB puts an adb on PATH whose getprop sys.boot_completed answers
// "0" for the first answersBeforeBoot calls and "1" after.
func fakeBootingADB(t *testing.T, answersBeforeBoot int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := `#!/bin/sh
n=$(cat "` + counter + `" 2>/dev/null || echo 0)
echo $((n + 1)) > "` + counter + `"
if [ "$n" -ge ` + fmt.Sprint(answersBeforeBoot) + ` ]; then echo 1; else echo 0; fi
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWaitForBootCompletedWaitsForAndroidNotJustAdb(t *testing.T) {
	fakeBootingADB(t, 1)
	start := time.Now()
	if err := waitForBootCompleted("emulator-5556", "wet-avd-1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("returned on the first poll, while the device still reported boot_completed=0")
	}
}

func TestWaitForBootCompletedTimesOut(t *testing.T) {
	fakeBootingADB(t, 9)
	err := waitForBootCompleted("emulator-5556", "wet-avd-1", time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not finish booting") {
		t.Fatalf("err = %v, want a timeout naming the AVD", err)
	}
}
