//go:build e2e

// Fake `opencode` for tier-1 E2E (#47): the worker shells out to `opencode run
// <prompt>` in an isolated worktree. This shim behaves per-role:
//
//   - developer / repair: WRITE a real file into the current worktree and commit
//     it with git, then emit the canned Developer JSON — so the workflow's
//     "agent must leave a local commit" invariant actually holds. The file path
//     is FAKE_DEV_PATH (default "src/change.txt").
//   - reviewer: output the next line from $FAKE_OPENCODE_DIR/reviewer.txt,
//     popping it (stateful), so a scenario can script REQUEST_CHANGES → APPROVE.
//   - analyst / estimator / others: emit the file's contents verbatim.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	// opencode is invoked as `opencode run [flags...] <prompt>`; the prompt is
	// always the final argument (flags like --model sit between run and it).
	args := os.Args[1:]
	prompt := ""
	if len(args) > 0 {
		prompt = args[len(args)-1]
	}
	role := roleFor(prompt)
	dir := os.Getenv("FAKE_OPENCODE_DIR")
	dumpEnv(dir)

	switch role {
	case "developer", "repair":
		commitWorktree(role)
		emitFile(dir, role+".txt")
	case "reviewer":
		emitNextLine(dir, "reviewer.txt")
	default:
		emitFile(dir, role+".txt")
	}
}

func commitWorktree(role string) {
	// Per-scenario dev path: a "devpath" file in FAKE_OPENCODE_DIR overrides the
	// default so scenarios can drive the §20 risk gate (worker is a separate
	// process, so t.Setenv does not reach it).
	path := ""
	if dir := os.Getenv("FAKE_OPENCODE_DIR"); dir != "" {
		for _, name := range []string{"devpath", "devpath.txt"} {
			if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
				if s := strings.TrimSpace(string(b)); s != "" {
					path = s
					break
				}
			}
		}
	}
	if path == "" {
		path = os.Getenv("FAKE_DEV_PATH")
	}
	if path == "" {
		path = "src/change.txt"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		_ = os.WriteFile(path, []byte("change by fake "+role+"\n"), 0o644)
	}
	for _, c := range [][]string{
		{"git", "add", "-A"},
		{"git", "-c", "user.name=e2e", "-c", "user.email=e2e@konoha", "commit", "-qm", "e2e change"},
	} {
		cmd := exec.Command(c[0], c[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			// commit may legitimately no-op if nothing changed; carry on
			_ = out
		}
	}
}

func emitFile(dir, name string) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		os.Exit(0)
	}
	io.Copy(os.Stdout, strings.NewReader(string(b)))
}

// emitNextLine prints then pops the first blank-line-separated paragraph of the
// file, so consecutive reviewer calls step through a scripted verdict sequence
// (each verdict block is one paragraph).
func emitNextLine(dir, name string) {
	p := filepath.Join(dir, name)
	body, err := os.ReadFile(p)
	if err != nil || len(body) == 0 {
		os.Exit(0)
	}
	paras := strings.SplitN(strings.TrimSpace(string(body)), "\n\n", 2)
	fmt.Print(paras[0] + "\n")
	if len(paras) == 2 {
		_ = os.WriteFile(p, []byte(paras[1]+"\n"), 0o644)
	} else {
		_ = os.WriteFile(p, nil, 0o644)
	}
}

func roleFor(prompt string) string {
	switch {
	case strings.Contains(prompt, "Issue Analyst"):
		return "analyst"
	case strings.Contains(prompt, "Estimator"):
		return "estimator"
	case strings.Contains(prompt, "Developer agent"):
		return "developer"
	case strings.Contains(prompt, "Reviewer agent"):
		return "reviewer"
	case strings.Contains(prompt, "Repair agent"):
		return "repair"
	case strings.Contains(prompt, "Completion agent"):
		return "completion"
	case strings.Contains(prompt, "Resolution agent"):
		return "resolution"
	case strings.Contains(prompt, "Splitter agent"):
		return "splitter"
	case strings.Contains(prompt, "AI Diagnosis"):
		return "diagnosis"
	}
	return "default"
}

// dumpEnv records the child environment once per invocation so the e2e suite can
// assert the §23 credential scrub reaches the agent in every runtime (#212).
func dumpEnv(dir string) {
	if dir == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "agent.env"), []byte(strings.Join(os.Environ(), "\n")), 0o644)
}
