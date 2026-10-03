package web

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ScriptReviewer starts reviews with scripts/run.sh --requested. The
// script's lock directory (holding its PID) tells whether any review is
// running, including scheduled ones started by launchd.
type ScriptReviewer struct {
	Script  string // path to run.sh
	Dir     string // working directory (the repo)
	Lock    string // run.sh's lock directory
	LogPath string // where the script's own output goes

	mu       sync.Mutex
	starting bool // started by us, lock not necessarily taken yet
}

// Running reports whether a review is in progress.
func (r *ScriptReviewer) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starting || r.locked()
}

func (r *ScriptReviewer) locked() bool {
	data, err := os.ReadFile(filepath.Join(r.Lock, "pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	// Signal 0 checks the process exists; EPERM means it does but isn't ours.
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Start launches a review in the background. It keeps running if the server
// stops, so the run is always recorded.
func (r *ScriptReviewer) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.starting || r.locked() {
		return ErrReviewRunning
	}
	out, err := os.OpenFile(r.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(r.Script, "--requested")
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // not killed by Ctrl-C on the server
	if err := cmd.Start(); err != nil {
		out.Close()
		return fmt.Errorf("start review: %w", err)
	}
	r.starting = true
	go func() {
		cmd.Wait()
		out.Close()
		r.mu.Lock()
		r.starting = false
		r.mu.Unlock()
	}()
	return nil
}
