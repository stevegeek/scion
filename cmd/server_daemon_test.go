// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// newBoolFlagCmd builds a throwaway command with a single bool flag and parses
// setArgs into it, so cmd.Flags().Changed reflects whether the flag was given.
func newBoolFlagCmd(t *testing.T, name string, setArgs ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	var v bool
	c.Flags().BoolVar(&v, name, false, "")
	c.SetArgs(setArgs)
	if err := c.Execute(); err != nil {
		t.Fatalf("parse %v: %v", setArgs, err)
	}
	return c
}

// TestAppendDaemonBoolFlagForwardsExplicitDisable is the regression guard for
// the workstation-mode dev-auth bug: `scion server start --dev-auth=false`
// re-exec'd itself as a --foreground child WITHOUT --dev-auth, so the child's
// applyWorkstationDefaults silently re-enabled it. The daemon arg builder must
// forward an explicitly-set flag as --flag=<value> so the disable survives.
func TestAppendDaemonBoolFlagForwardsExplicitDisable(t *testing.T) {
	// Explicit --dev-auth=false → forwarded as --dev-auth=false (the fix).
	c := newBoolFlagCmd(t, "dev-auth", "--dev-auth=false")
	assert.Equal(t, []string{"--dev-auth=false"}, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Explicit --dev-auth=true → forwarded as --dev-auth=true.
	c = newBoolFlagCmd(t, "dev-auth", "--dev-auth=true")
	assert.Equal(t, []string{"--dev-auth=true"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value true (a workstation default) → historical bare form.
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Equal(t, []string{"--dev-auth"}, appendDaemonBoolFlag(c, nil, "dev-auth", true))

	// Not set, value false → nothing appended (unchanged behavior).
	c = newBoolFlagCmd(t, "dev-auth")
	assert.Nil(t, appendDaemonBoolFlag(c, nil, "dev-auth", false))

	// Appends onto existing args rather than replacing them.
	c = newBoolFlagCmd(t, "enable-web", "--enable-web=false")
	assert.Equal(t, []string{"server", "start", "--enable-web=false"},
		appendDaemonBoolFlag(c, []string{"server", "start"}, "enable-web", false))
}
