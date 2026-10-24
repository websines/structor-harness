package harnesseval

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { code := m.Run(); CleanupToolchain(); os.Exit(code) }
