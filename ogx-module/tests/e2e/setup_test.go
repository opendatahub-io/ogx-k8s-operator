//nolint:testpackage
package e2e

import (
	"os"
	"testing"
)

var (
	TestEnv *TestEnvironment
)

func TestMain(m *testing.M) {
	var err error
	TestEnv, err = SetupTestEnv()
	if err != nil {
		os.Exit(1)
	}

	code := m.Run()

	CleanupTestEnv(TestEnv)

	os.Exit(code)
}
