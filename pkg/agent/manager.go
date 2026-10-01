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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type Manager interface {
	// Provision prepares the agent directory and configuration without starting it
	Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Reprovision re-renders an existing agent's on-disk configuration from
	// the current template/harness-config catalog (a `scion reincarnate`
	// request), preserving its home directory and clone-per-agent workspace.
	// See AgentManager.Reprovision for the full contract.
	Reprovision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Start launches a new agent with the given configuration
	Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)

	// Stop terminates an agent
	Stop(ctx context.Context, agentID string, projectPath string) error

	// Delete terminates and removes an agent, resolving agentID by slug
	// (scoped by projectPath when given). It fails closed when the slug is
	// ambiguous.
	Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error)

	// DeleteTarget terminates and removes an agent the caller has already
	// resolved to a specific runtime entry (containerID, may be empty for a
	// file-only agent) and project path. It never re-resolves by slug.
	DeleteTarget(ctx context.Context, agentName, containerID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error)

	// List returns active agents
	List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)

	// Message sends a message to an agent's harness via tmux.
	// projectID scopes delivery to a specific project, preventing cross-project
	// collision when agents share the same slug.
	Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error

	// MessageRaw sends literal bytes to an agent's tmux session via send-keys
	// with no trailing Enter keypresses, allowing control sequences like
	// arrow keys and Escape to be used directly.
	// projectID scopes delivery to a specific project.
	MessageRaw(ctx context.Context, agentID, projectID string, keys string) error

	// Watch returns a channel of status updates for an agent
	Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error)

	// Close flushes any pending message buffers. Must be called before
	// the process exits to ensure buffered messages are delivered.
	Close()
}

type AgentManager struct {
	Runtime   runtime.Runtime
	msgBuffer *MessageBuffer
}

// defaultBufferDelay is the debounce window for message delivery.
// Messages arriving within this window are coalesced into a single delivery.
const defaultBufferDelay = 2 * time.Second

// msgBufferPrefix is the base name for the named tmux buffer used to load
// message text via stdin before pasting it into an agent's session. Using a
// named buffer (rather than the default buffer) avoids clobbering unrelated
// tmux buffer usage and lets paste-buffer delete it immediately after use
// (-d). Each delivery appends a unique suffix (see nextMsgBufferName) so that
// two deliveries to the same agent overlapping in time — an interrupt racing
// a buffered flush, or two retries — never share a buffer: with a single
// fixed name, one delivery's paste can consume the buffer loaded for the
// other, silently swapping which message reaches the terminal and which
// delivery is reported as failed (ptone/scion#2265).
const msgBufferPrefix = "scion-msg"

// msgBufferSeq generates the per-delivery suffix for msgBufferPrefix. A
// counter alone is only unique within the process that owns it: every
// AgentManager in one broker process shares this package-level counter, so
// it tells their deliveries apart, but a short-lived CLI invocation (scion
// message, scion broadcast) starts a fresh process whose counter restarts at
// 1, so two such processes — two concurrent CLI calls, or a CLI call racing
// the broker, against the same agent — would otherwise both name their first
// delivery "scion-msg-1". msgBufferNonce below supplies the part that tells
// processes apart; the counter only needs to tell apart deliveries within
// one process.
var msgBufferSeq atomic.Uint64

// msgBufferNonce is a random value generated once per process (not once per
// call) so that every buffer name that process produces carries it. It is
// what makes nextMsgBufferName unique across processes: two processes each
// generate their own nonce independently, so their names never collide even
// though each process's msgBufferSeq counter restarts at 1. A PID is not
// enough for this, since containerized brokers can share PID 1.
var msgBufferNonce = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively unrecoverable on any supported
		// platform; fail loudly rather than silently falling back to a
		// predictable, collision-prone name.
		panic(fmt.Sprintf("agent: reading random buffer-name nonce: %v", err))
	}
	return hex.EncodeToString(b[:])
})

// nextMsgBufferName returns a short, unique tmux buffer name for a single
// deliverImmediate call. It must be computed once per call and reused for
// every step of that call (load-buffer, paste-buffer, and any cleanup),
// never regenerated mid-delivery.
func nextMsgBufferName() string {
	return fmt.Sprintf("%s-%s-%d", msgBufferPrefix, msgBufferNonce(), msgBufferSeq.Add(1))
}

// loadBufferArgv builds the argv that loads message text into the named tmux
// buffer bufName via stdin. Shared by deliverImmediate and the real-tmux
// integration test so the test exercises the exact argv delivery uses.
func loadBufferArgv(bufName string) []string {
	return []string{"tmux", "load-buffer", "-b", bufName, "-"}
}

// pasteBufferArgv builds the argv that pastes the named tmux buffer bufName
// into target with bracketed paste, deleting the buffer on success. Shared by
// deliverImmediate and the real-tmux integration test so the test exercises
// the exact argv delivery uses.
func pasteBufferArgv(target, bufName string) []string {
	return []string{"tmux", "paste-buffer", "-t", target, "-p", "-d", "-b", bufName}
}

// submitBufferPrefix is the base name for the named tmux buffer that holds
// the CR used to submit a delivered message. Like msgBufferPrefix it is made
// unique per delivery (see submitBufferName), so two overlapping deliveries to
// one agent can never paste, and with -d consume, each other's CR.
const submitBufferPrefix = "scion-submit"

// submitBufferName derives the submit buffer name for the delivery whose
// message buffer is msgBufName, reusing its unique nonce-and-sequence suffix.
func submitBufferName(msgBufName string) string {
	return submitBufferPrefix + strings.TrimPrefix(msgBufName, msgBufferPrefix)
}

// submitArgvs returns the commands that submit whatever is in the harness's
// input without disturbing copy-mode. A pane in copy-mode dispatches
// send-keys through the mode's key table, so a send-keys Enter never reaches
// the harness; paste-buffer writes to the pane's pty directly and bypasses
// the mode, so the operator's scroll position and selection also survive.
//
// A literal CR rather than LF, so this does not depend on paste-buffer's
// LF->CR default, and no -p: a bracketed paste would hand the harness the CR
// as pasted text instead of a submit. -d consumes the buffer as it is pasted,
// so each submit sets it afresh and a named buffer never lingers on top of
// the operator's buffer stack.
func submitArgvs(bufName string) [][]string {
	return [][]string{
		{"tmux", "set-buffer", "-b", bufName, "--", "\r"},
		{"tmux", "paste-buffer", "-t", "scion:0", "-d", "-b", bufName},
	}
}

// exitCopyModeArgv returns the tmux command that leaves copy-mode. Real keys
// (interrupts, raw sequences, the bare Enter) are dispatched through the
// mode's key table rather than to the harness, so those paths cancel the mode
// first. -q is a no-op when the pane is not in a mode. Callers treat its
// failure as non-fatal: tmux before 3.1 has no -q, and failing a delivery
// over it would lose input that older tmux would otherwise have taken.
func exitCopyModeArgv() []string {
	return []string{"tmux", "copy-mode", "-q", "-t", "scion:0"}
}

func NewManager(rt runtime.Runtime) Manager {
	mgr := &AgentManager{
		Runtime: rt,
	}
	// Initialize the message buffer with a debounce delay. The buffer's
	// delivery function calls back into deliverImmediate to perform the
	// actual tmux send-keys when the debounce window expires.
	mgr.msgBuffer = NewMessageBuffer(defaultBufferDelay, func(agentID, projectID string, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	return mgr
}

func (m *AgentManager) Close() {
	m.msgBuffer.Close()
}

// resolveProjectName maps a project path to the project name used to scope a
// slug lookup. It returns "" when no path is given or it cannot be resolved.
func resolveProjectName(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	if resolvedDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
		return config.GetProjectName(resolvedDir)
	}
	return ""
}

// agentHasProjectInfo reports whether a runtime entry carries any project
// identity (label or field) that can be compared against a requested project.
func agentHasProjectInfo(a api.AgentInfo) bool {
	return projectkeys.ProjectIDFromLabels(a.Labels) != "" ||
		projectkeys.ProjectNameFromLabels(a.Labels) != "" ||
		a.ProjectID != "" || a.Project != ""
}

// selectAgentTarget picks the single runtime entry that agentID refers to.
//
// When projectName is set, entries that carry project info must match it;
// entries without any project info are accepted only when no entry with
// matching project info exists (backward compatibility with pre-label
// containers). It never falls back to an entry that is labelled for a
// different project.
//
// It fails closed with an error when more than one distinct entry remains,
// instead of silently acting on whichever one the runtime listed first
// (ptone/scion#1819). found is false when nothing matches.
func selectAgentTarget(agents []api.AgentInfo, agentID, projectName string) (target api.AgentInfo, found bool, err error) {
	var scoped, unlabeled []api.AgentInfo
	for _, a := range agents {
		if !agentMatchesName(a, agentID) {
			continue
		}
		if projectName == "" {
			scoped = append(scoped, a)
			continue
		}
		if !agentHasProjectInfo(a) {
			unlabeled = append(unlabeled, a)
			continue
		}
		if matchAgentProject(a, projectName, "") {
			scoped = append(scoped, a)
		}
	}
	candidates := scoped
	if len(candidates) == 0 {
		candidates = unlabeled
	}
	candidates = dedupeByContainerID(candidates)
	switch len(candidates) {
	case 0:
		return api.AgentInfo{}, false, nil
	case 1:
		return candidates[0], true, nil
	default:
		return api.AgentInfo{}, false, fmt.Errorf("agent '%s' is ambiguous: %d containers match; specify the project", agentID, len(candidates))
	}
}

// agentMatchesName reports whether a runtime entry refers to agentID by name
// (case-insensitively) or container ID.
func agentMatchesName(a api.AgentInfo, agentID string) bool {
	return a.Name == agentID || a.ContainerID == agentID ||
		strings.TrimPrefix(a.Name, "/") == agentID ||
		strings.EqualFold(a.Name, agentID)
}

func dedupeByContainerID(agents []api.AgentInfo) []api.AgentInfo {
	if len(agents) < 2 {
		return agents
	}
	seen := make(map[string]bool, len(agents))
	out := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		key := a.ContainerID
		if key == "" {
			key = "name:" + a.Name + "|" + a.ProjectID + "|" + a.Project
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

func (m *AgentManager) Stop(ctx context.Context, agentID string, projectPath string) error {
	// Resolve the agent name to a container ID so that runtimes which do
	// not support lookup-by-name (e.g. Apple's `container` CLI) receive
	// the actual container ID.  This mirrors the resolution logic in Delete().
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	if err == nil {
		target, found, selErr := selectAgentTarget(agents, agentID, resolveProjectName(projectPath))
		if selErr != nil {
			return selErr
		}
		if found {
			return m.Runtime.Stop(ctx, target.ContainerID)
		}
	}
	// Fallback: agentID may already be a container ID, or the list
	// failed — pass it through directly.
	return m.Runtime.Stop(ctx, agentID)
}

func (m *AgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	// 1. Check if container exists
	// We use name filter if possible, but runtime.List might take map[string]string
	util.Debugf("delete: listing containers in mgr.Delete for %s", agentID)
	listStart := time.Now()
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	util.Debugf("delete: mgr.Delete container list completed in %v", time.Since(listStart))
	var targetID string
	if err == nil {
		// Resolve project name from projectPath (if provided) to scope the
		// container lookup; refuse ambiguous matches rather than picking one.
		target, found, selErr := selectAgentTarget(agents, agentID, resolveProjectName(projectPath))
		if selErr != nil {
			return false, selErr
		}
		if found {
			targetID = target.ContainerID
		}
	}
	return m.deleteResolved(ctx, agentID, targetID, deleteFiles, projectPath, removeBranch)
}

// DeleteTarget deletes an agent that the caller has already resolved to a
// specific runtime entry. Unlike Delete it performs no slug re-resolution, so
// it cannot drift to a same-slug agent in another project. containerID may be
// empty for an agent that has files but no backing container; projectPath is
// used verbatim for file deletion.
func (m *AgentManager) DeleteTarget(ctx context.Context, agentName, containerID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	return m.deleteResolved(ctx, agentName, containerID, deleteFiles, projectPath, removeBranch)
}

func (m *AgentManager) deleteResolved(ctx context.Context, agentName, targetID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if targetID != "" {
		// Stop the container gracefully before force-removing it. This ensures
		// bind mounts (e.g. shared-dir volumes) are properly released before
		// filesystem cleanup. Without this, docker rm -f / container kill sends
		// SIGKILL which can leave mounts in a state that causes permission
		// errors when DeleteAgentFiles tries to remove the agent directory.
		util.Debugf("delete: stopping container %s before removal", targetID)
		if err := m.Runtime.Stop(ctx, targetID); err != nil {
			// Log but don't fail — the container may already be stopped,
			// and Delete (force-remove) will handle it either way.
			util.Debugf("delete: stop returned error (continuing): %v", err)
		}

		util.Debugf("delete: starting runtime delete for container %s", targetID)
		if err := m.Runtime.Delete(ctx, targetID); err != nil {
			return false, fmt.Errorf("failed to delete container: %w", err)
		}
		util.Debugf("delete: runtime delete completed for container %s", targetID)
	}

	if deleteFiles {
		util.Debugf("delete: starting filesystem cleanup for agent %s", agentName)
		branchDeleted, err := DeleteAgentFiles(agentName, projectPath, removeBranch)
		util.Debugf("delete: filesystem cleanup completed for agent %s", agentName)
		return branchDeleted, err
	}
	return false, nil
}

func (m *AgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, fmt.Errorf("Watch not implemented")
}

func (m *AgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// Interrupt messages bypass the buffer entirely — they need to send
	// Ctrl+C immediately to get the agent's attention, and the accompanying
	// message (if any) should follow without delay.
	if interrupt {
		return m.deliverImmediate(ctx, agentID, projectID, message, interrupt)
	}

	// Non-interrupt messages go through the debounce buffer. This ensures
	// that a rapid burst of messages (e.g. from multiple senders or broadcast
	// fan-out) is coalesced into a single delivery, avoiding contention on
	// the agent's tmux input.
	// A failure handler on ctx (set by the runtime broker) is invoked if the
	// buffered delivery later fails, so the hub can mark the message failed
	// rather than leave it "dispatched" (#1820).
	m.msgBuffer.SendWithFailureHandler(agentID, projectID, message, DeliveryFailureHandlerFromContext(ctx))
	return nil
}

// MessageRaw sends literal bytes to an agent's tmux session via send-keys
// with no trailing Enter keypresses. This bypasses the paste buffer and
// debounce buffer, sending directly via tmux send-keys so that control
// sequences (arrow keys, Escape, etc.) are interpreted by the terminal.
// Leaves copy-mode first (best-effort), since send-keys is dispatched through
// the mode's key table rather than to the harness.
func (m *AgentManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentID) {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' not found or not running", agentID)
	}

	// Best-effort: see exitCopyModeArgv.
	_, _ = m.Runtime.Exec(ctx, agent.ContainerID, exitCopyModeArgv())

	cmd := []string{"tmux", "send-keys", "-t", "scion:0", "--", keys}
	if _, err := m.Runtime.Exec(ctx, agent.ContainerID, cmd); err != nil {
		return fmt.Errorf("failed to send raw keys to agent '%s': %w", agent.Name, err)
	}

	return nil
}

// deliveryStepKind identifies how deliverImmediate must run a deliveryStep,
// replacing dispatch on a step's argv contents (e.g. cmd[1] == "load-buffer")
// with an explicit tag set once when the step is built.
type deliveryStepKind int

const (
	// stepSendKeys runs argv via Exec: an interrupt key or a bare Enter.
	stepSendKeys deliveryStepKind = iota
	// stepLoadBuffer runs argv via ExecWithStdin, streaming the message body.
	stepLoadBuffer
	// stepPasteBuffer runs argv via Exec. Its success marks the message as
	// delivered; its failure triggers best-effort buffer cleanup.
	stepPasteBuffer
	// stepExitCopyMode runs argv via Exec before real keys are sent. Its
	// failure is tolerated (see exitCopyModeArgv).
	stepExitCopyMode
	// stepSubmit submits the pasted message with a pasted CR (submitArgvs);
	// argv is unused. It replaces a send-keys Enter, which a pane in
	// copy-mode would swallow.
	stepSubmit
)

// deliveryStep is one command in a deliverImmediate call.
type deliveryStep struct {
	kind deliveryStepKind
	argv []string
}

// deliverImmediate sends a message to an agent's tmux session right now,
// bypassing the message buffer. This is the low-level delivery mechanism
// used both for interrupt messages (called directly) and for buffered
// messages (called by the MessageBuffer when the debounce timer fires).
func (m *AgentManager) deliverImmediate(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// 1. Find the agent, scoped to project to prevent cross-project delivery
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentID) {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' not found or not running", agentID)
	}

	// 2. Resolve harness — probe both layouts (worktree vs shared-workspace
	// per .design/hub-shared-workspace-isolation.md) since the mode isn't
	// passed through this lookup path.
	harnessName := "generic"
	if agent.ProjectPath != "" {
		projectDir, _ := config.GetResolvedProjectDir(agent.ProjectPath)
		if projectDir == "" {
			projectDir = agent.ProjectPath
		}
		scionJSON := filepath.Join(config.ResolveAgentDir(projectDir, agent.Name), "scion-agent.json")
		if data, err := os.ReadFile(scionJSON); err == nil {
			var cfg api.ScionConfig
			if err := json.Unmarshal(data, &cfg); err == nil && cfg.Harness != "" {
				harnessName = cfg.Harness
			}
		}
	}
	h := harness.New(harnessName)

	// 3. Prepare commands
	var steps []deliveryStep

	// Only the paths that send real KEYS need the mode cancelled; the message
	// path submits via paste instead and leaves a reading operator alone.
	if interrupt || message == "" {
		steps = append(steps, deliveryStep{kind: stepExitCopyMode, argv: exitCopyModeArgv()})
	}

	if interrupt {
		if seq := h.GetInterruptSequence(); len(seq) > 0 {
			for _, key := range seq {
				steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", key}})
			}
		} else {
			key := h.GetInterruptKey()
			steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", key}})
		}
	}

	// bufName names the tmux buffer used below, if this delivery pastes a
	// message. It is computed once (nextMsgBufferName) and reused for both
	// the load and paste steps, and for cleanup if the paste step fails.
	// submitBuf names this delivery's CR buffer and shares bufName's suffix.
	var bufName, submitBuf string

	if message == "" {
		// Empty messages send a bare Enter keypress to trigger confirmations
		steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", "Enter"}})
	} else {
		// Use tmux paste buffer with bracketed paste (-p) instead of send-keys.
		// send-keys simulates typing character-by-character, which allows TUI
		// applications to intercept special characters as hotkeys (e.g., Gemini
		// CLI treats '!' as a shell-mode toggle). Bracketed paste wraps the
		// content in escape sequences (\e[200~...\e[201~) that signal the
		// application to treat all characters as literal pasted text.
		//
		// The message is loaded into a named buffer via stdin rather than
		// passed as a "tmux set-buffer" argv element: tmux's client-server
		// protocol caps a single command's argv at 16 KB, and a coalesced
		// batch of debounced messages can exceed that (ptone/scion#2256).
		// Streaming it over stdin has no such limit.
		//
		// The buffer name is unique per delivery (ptone/scion#2265): two
		// deliveries to the same agent are not otherwise serialised, and a
		// shared fixed name lets one delivery's paste consume the buffer
		// loaded for the other. "-d" removes the per-delivery buffer after a
		// successful paste; on paste failure it is deleted explicitly below,
		// since "-d" does not run when paste-buffer itself fails.
		//
		// The paste is submitted with a pasted CR (stepSubmit), not a
		// send-keys Enter, so that a pane left in copy-mode by a scroll still
		// receives it.
		bufName = nextMsgBufferName()
		submitBuf = submitBufferName(bufName)
		steps = append(steps, deliveryStep{kind: stepLoadBuffer, argv: loadBufferArgv(bufName)})
		steps = append(steps, deliveryStep{kind: stepPasteBuffer, argv: pasteBufferArgv("scion:0", bufName)})
		steps = append(steps, deliveryStep{kind: stepSubmit})
	}

	// 4. Execute. Once "tmux paste-buffer" succeeds, the message content is
	// already sitting in the agent's terminal input — a later failure (the
	// closing Enter, or one of the confirmation Enters below) must not
	// trigger a retry of the whole delivery, or the retried paste-buffer
	// would duplicate the already-visible text (ptone/scion#1866). Errors
	// from that point on are wrapped in PartialDeliveryError so the message
	// buffer's bounded retry knows not to retry them.
	delivered := false
	for _, step := range steps {
		var err error
		switch step.kind {
		case stepLoadBuffer:
			_, err = m.Runtime.ExecWithStdin(ctx, agent.ContainerID, step.argv, strings.NewReader(message))
		case stepSubmit:
			err = m.submit(ctx, agent.ContainerID, submitBuf)
		case stepExitCopyMode:
			// Best-effort: see exitCopyModeArgv.
			_, _ = m.Runtime.Exec(ctx, agent.ContainerID, step.argv)
		default:
			_, err = m.Runtime.Exec(ctx, agent.ContainerID, step.argv)
		}
		if err != nil {
			if step.kind == stepPasteBuffer {
				// load-buffer succeeded (or this step wouldn't have run), but
				// the paste itself failed, so paste-buffer's own "-d" never
				// fired to clean up the per-delivery buffer named above.
				m.deleteBufferBestEffort(ctx, agent.ContainerID, bufName)
			}
			wrapped := fmt.Errorf("failed to send message to agent '%s': %w", agent.Name, err)
			if delivered {
				return &PartialDeliveryError{Err: wrapped}
			}
			return wrapped
		}
		if step.kind == stepPasteBuffer {
			delivered = true
		}
	}

	// After sending a message, submit twice more with a brief delay to ensure
	// the input is accepted by the agent. Like the first, these submits are
	// pasted CRs rather than send-keys Enters (see submitArgvs). This runs
	// only once the message (if any) has already been pasted, so any failure
	// here is necessarily partial delivery too.
	if message != "" {
		for range 2 {
			select {
			case <-ctx.Done():
				return &PartialDeliveryError{Err: fmt.Errorf("context canceled before sending Enter to agent '%s': %w", agent.Name, ctx.Err())}
			case <-time.After(300 * time.Millisecond):
			}
			if err := m.submit(ctx, agent.ContainerID, submitBuf); err != nil {
				return &PartialDeliveryError{Err: fmt.Errorf("failed to send Enter to agent '%s': %w", agent.Name, err)}
			}
		}
	}

	return nil
}

// submit runs submitArgvs for the named CR buffer. If the paste fails, its
// -d never fires, so the buffer is deleted explicitly (best-effort) to keep
// named buffers from accumulating on the tmux server.
func (m *AgentManager) submit(ctx context.Context, containerID, bufName string) error {
	argvs := submitArgvs(bufName)
	if _, err := m.Runtime.Exec(ctx, containerID, argvs[0]); err != nil {
		return err
	}
	if _, err := m.Runtime.Exec(ctx, containerID, argvs[1]); err != nil {
		m.deleteBufferBestEffort(ctx, containerID, bufName)
		return err
	}
	return nil
}

// deleteBufferBestEffort deletes the named tmux buffer after a failed
// paste-buffer, whose own "-d" did not fire. Named buffers aren't evicted by
// buffer-limit, so without this they would accumulate on the tmux server.
// The failure is already being reported by the caller, so a further error
// here is ignored. Uses a ctx detached from the caller's
// (context.WithoutCancel, with its own short timeout) so the cleanup still
// runs when the caller's ctx is already cancelled, which may be why the
// paste itself failed.
func (m *AgentManager) deleteBufferBestEffort(ctx context.Context, containerID, bufName string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = m.Runtime.Exec(cleanupCtx, containerID, []string{"tmux", "delete-buffer", "-b", bufName})
}

func matchesAgentID(a api.AgentInfo, id string) bool {
	return a.Name == id || a.ContainerID == id || strings.TrimPrefix(a.Name, "/") == id
}
