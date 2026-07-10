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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// workspaceMountSource returns the /workspace volume mount source from a
// provisioned config, or "" if no such mount exists.
func workspaceMountSource(cfg *api.ScionConfig) string {
	if cfg == nil {
		return ""
	}
	for _, v := range cfg.Volumes {
		if v.Target == "/workspace" {
			return v.Source
		}
	}
	return ""
}

// TestProvisionAgentWorkspaceSubdir: a within-project subpath survives to the
// /workspace mount, the default still mounts the project root, traversal is rejected.
func TestProvisionAgentWorkspaceSubdir(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	evalProjectDir, _ := filepath.EvalSymlinks(projectDir)

	// A subdirectory within the project workspace root.
	subRel := filepath.Join("packages", "web")
	if err := os.MkdirAll(filepath.Join(evalProjectDir, subRel), 0755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	wantSub, _ := filepath.EvalSymlinks(filepath.Join(evalProjectDir, subRel))

	t.Run("subdir survives to the mount", func(t *testing.T) {
		ctx := api.ContextWithWorkspaceSubdir(context.Background(), subRel)
		_, _, cfg, err := ProvisionAgent(ctx, "subdir-agent", "default", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		src := workspaceMountSource(cfg)
		if src == "" {
			t.Fatal("expected /workspace volume mount, found none")
		}
		evalSrc, _ := filepath.EvalSymlinks(src)
		if evalSrc != wantSub {
			t.Errorf("mount source = %q, want subdir %q", evalSrc, wantSub)
		}
	})

	t.Run("no subdir mounts the project root unchanged", func(t *testing.T) {
		_, _, cfg, err := ProvisionAgent(context.Background(), "root-agent", "default", "", "", projectScionDir, "", "", "", "")
		if err != nil {
			t.Fatalf("ProvisionAgent failed: %v", err)
		}
		src := workspaceMountSource(cfg)
		if src == "" {
			t.Fatal("expected /workspace volume mount, found none")
		}
		evalSrc, _ := filepath.EvalSymlinks(src)
		if evalSrc != evalProjectDir {
			t.Errorf("mount source = %q, want project root %q", evalSrc, evalProjectDir)
		}
	})

	t.Run("traversal subdir is rejected", func(t *testing.T) {
		ctx := api.ContextWithWorkspaceSubdir(context.Background(), filepath.Join("..", "..", "etc"))
		_, _, _, err := ProvisionAgent(ctx, "escape-agent", "default", "", "", projectScionDir, "", "", "", "")
		if err == nil {
			t.Fatal("expected ProvisionAgent to reject a traversal subdir, got nil error")
		}
	})
}

// TestProvisionAgentWorkspaceSubdirResumePlainMount covers resume: provisioning
// persists ExplicitWorkspace=true, which resume reads to keep the mount plain
// (the subdir) rather than widening to the enclosing git repo. The paired
// explicit=false call shows the widening that flag prevents.
func TestProvisionAgentWorkspaceSubdirResumePlainMount(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Git monorepo as the project's externalized workspace root; projectDir itself
	// is NOT git, so provisioning takes the Case-3 branch where WorkspaceSubdir applies.
	monorepo := filepath.Join(tmpDir, "monorepo")
	if err := os.MkdirAll(monorepo, 0755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, monorepo)
	realMonorepo, _ := filepath.EvalSymlinks(monorepo)
	subRel := filepath.Join("packages", "web")
	if err := os.MkdirAll(filepath.Join(realMonorepo, subRel), 0755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	wantSub, _ := filepath.EvalSymlinks(filepath.Join(realMonorepo, subRel))

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := config.InitProject(projectScionDir, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}
	// Point the (non-git) project's externalized workspace at the git monorepo.
	if err := config.ReconnectProject(projectScionDir, realMonorepo); err != nil {
		t.Fatalf("ReconnectProject failed: %v", err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	ctx := api.ContextWithWorkspaceSubdir(context.Background(), subRel)
	_, _, cfg, err := ProvisionAgent(ctx, "subdir-agent", "default", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent failed: %v", err)
	}

	// The mount source is the nested subdir, not the monorepo root.
	evalSrc, _ := filepath.EvalSymlinks(workspaceMountSource(cfg))
	if evalSrc != wantSub {
		t.Fatalf("mount source = %q, want subdir %q", evalSrc, wantSub)
	}

	// (1) ExplicitWorkspace is persisted in the agent's scion-agent.json.
	resolvedProjectDir, err := config.GetResolvedProjectDir(projectScionDir)
	if err != nil {
		t.Fatalf("GetResolvedProjectDir: %v", err)
	}
	agentCfgPath := filepath.Join(config.GetAgentDir(resolvedProjectDir, "subdir-agent", false), "scion-agent.json")
	raw, err := os.ReadFile(agentCfgPath)
	if err != nil {
		t.Fatalf("read persisted scion-agent.json: %v", err)
	}
	var persisted api.ScionConfig
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal persisted config: %v", err)
	}
	if !persisted.ExplicitWorkspace {
		t.Fatalf("persisted scion-agent.json ExplicitWorkspace = false, want true (resume would re-widen the subdir)")
	}

	// (2) Resume re-derives explicit=true from the persisted flag; with the
	// subdir sitting inside the git monorepo, the mount stays plain (no widening).
	if got := detectRepoRoot(persisted.ExplicitWorkspace, wantSub, projectScionDir); got != "" {
		t.Fatalf("resume: detectRepoRoot widened the subdir to %q, want \"\" (plain mount)", got)
	}
	// Sanity: without the flag, the same subdir WOULD widen to the repo root,
	// confirming the persisted flag is what prevents the leak.
	if got := detectRepoRoot(false, wantSub, projectScionDir); got == "" {
		t.Fatal("expected detectRepoRoot to widen the in-repo subdir when explicit=false, got \"\"")
	}
}

// TestResolveWorkspaceSubdir: a within-project subpath is honored; an escape
// (absolute, "..", or symlink pointing outside the root) is rejected.
func TestResolveWorkspaceSubdir(t *testing.T) {
	base := t.TempDir()
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks(base): %v", err)
	}

	// Valid nested subdir within the project workspace.
	nested := filepath.Join(realBase, "a", "b")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	// A directory OUTSIDE the project workspace root, plus a symlink inside the
	// root that points at it — the classic symlink-escape.
	outside := t.TempDir()
	realOutside, _ := filepath.EvalSymlinks(outside)
	escapeLink := filepath.Join(realBase, "escape")
	if err := os.Symlink(realOutside, escapeLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	t.Run("valid nested subdir is honored", func(t *testing.T) {
		got, err := resolveWorkspaceSubdir(realBase, filepath.Join("a", "b"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want, _ := filepath.EvalSymlinks(nested)
		if got != want {
			t.Errorf("resolved = %q, want %q", got, want)
		}
	})

	t.Run("subdir equal to root is honored", func(t *testing.T) {
		got, err := resolveWorkspaceSubdir(realBase, ".")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != realBase {
			t.Errorf("resolved = %q, want %q", got, realBase)
		}
	})

	t.Run("absolute subdir is rejected", func(t *testing.T) {
		if _, err := resolveWorkspaceSubdir(realBase, realOutside); err == nil {
			t.Fatal("expected error for absolute subdir, got nil")
		}
	})

	t.Run("parent traversal is rejected", func(t *testing.T) {
		if _, err := resolveWorkspaceSubdir(realBase, ".."); err == nil {
			t.Fatal("expected error for '..' subdir, got nil")
		}
	})

	t.Run("nested traversal escape is rejected", func(t *testing.T) {
		if _, err := resolveWorkspaceSubdir(realBase, filepath.Join("a", "..", "..", "elsewhere")); err == nil {
			t.Fatal("expected error for traversal escape, got nil")
		}
	})

	t.Run("symlink escape is rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink escape test not meaningful on windows")
		}
		if _, err := resolveWorkspaceSubdir(realBase, "escape"); err == nil {
			t.Fatal("expected error for symlink escape, got nil")
		}
	})

	t.Run("nonexistent subdir is rejected", func(t *testing.T) {
		if _, err := resolveWorkspaceSubdir(realBase, "does-not-exist"); err == nil {
			t.Fatal("expected error for missing subdir, got nil")
		}
	})
}
