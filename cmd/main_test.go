package cmd

import (
	"os"
	"testing"
)

// TestMain points the machine registry and lock at a throwaway directory, so
// no test reads or writes the real one under ~/.local/state/wet.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wet-machine-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv(machineStateDirEnv, dir)
	// Slot allocation asks Docker who holds which port. Tests that care stub
	// it with their own containers; the rest see a machine with none.
	listDockerContainerPorts = func() []dockerContainerPorts { return nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
