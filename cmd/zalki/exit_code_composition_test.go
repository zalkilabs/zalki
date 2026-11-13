package main

import (
	"errors"
	"fmt"
	"testing"
)

// The root exit mapping composes ZAL-15's update marker (2) with the
// supervisor's two outcomes (75, 0). Every marker counts only by direct
// identity: joined with a cleanup error or wrapped, it is an ordinary
// failure (1), and none is swallowed by errors.Join.
func TestExitCodeComposition(t *testing.T) {
	cleanup := errors.New("cleanup failed")
	for name, test := range map[string]struct {
		err  error
		want int
	}{
		"update available":             {errPluginUpdateAvailable, 2},
		"zalki replaced":               {errZalkiReplaced, exitReplaced},
		"unsafe plugins folder":        {errUnsafePluginsDir, 0},
		"ordinary":                     {errors.New("ordinary"), 1},
		"update joined with cleanup":   {errors.Join(errPluginUpdateAvailable, cleanup), 1},
		"replaced joined with cleanup": {errors.Join(errZalkiReplaced, cleanup), 1},
		"unsafe joined with cleanup":   {errors.Join(errUnsafePluginsDir, cleanup), 1},
		"replaced wrapped":             {fmt.Errorf("stop: %w", errZalkiReplaced), 1},
	} {
		if got := commandExitCode(test.err); got != test.want {
			t.Errorf("%s: exit %d, want %d", name, got, test.want)
		}
	}
}
