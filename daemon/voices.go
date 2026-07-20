package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// voiceManager orchestrates the OPTIONAL high-quality voice path -- a Python
// sidecar (Kokoro neural TTS). Python stays here because that's where the ML
// engine lives; the daemon CORE is pure Go and only shells out when the operator
// opts in. It (1) reports whether the Kokoro weights are installed, (2) runs the
// one-time downloader on request while streaming its progress, and (3) starts /
// stops the speaker process.
//
// SECURITY: the two scripts it spawns are FIXED repo paths (scripts/download_voices.py,
// agents/speaker.py) with NO request-derived arguments -- no command injection. The
// endpoints that drive this are POST + CSRF-gated by securityMiddleware, so a
// cross-site page can't trigger a download or spawn the speaker.
type voiceManager struct {
	repoRoot  string
	pythonBin string

	mu        sync.Mutex
	dlState   string // "idle" | "downloading" | "done" | "error"
	dlLog     string // last progress line from the downloader
	dlErr     string
	speaker   *exec.Cmd
	speakerOn bool

	engineChecked bool // cached result of the kokoro_onnx importability probe
	engineReady   bool
}

func newVoiceManager(repoRoot string) *voiceManager {
	py := os.Getenv("FLEETCHAT_PYTHON") // lets an operator point at a specific interpreter (venv, py -3, ...)
	if py == "" {
		py = "python"
	}
	return &voiceManager{repoRoot: repoRoot, pythonBin: py, dlState: "idle"}
}

func (vm *voiceManager) weightPaths() (onnx, bin string) {
	dir := filepath.Join(vm.repoRoot, "data", "voices")
	return filepath.Join(dir, "kokoro-v1.0.onnx"), filepath.Join(dir, "voices-v1.0.bin")
}

// Installed reports whether the high-quality path is actually USABLE: both Kokoro
// weight files present AND the Python engine importable. Weights alone aren't
// enough -- speaker.py needs kokoro_onnx + soundfile, so reporting "installed" on
// weights-only spawns a speaker that instantly exits (the bug this fixes).
func (vm *voiceManager) Installed() bool {
	onnx, bin := vm.weightPaths()
	if !(fileBig(onnx) && fileBig(bin)) {
		return false
	}
	return vm.engineOK()
}

// engineOK reports (cached) whether the Kokoro Python engine imports. Cached
// because /control/voices is polled; the cache clears after a download so a
// freshly pip-installed engine is picked up. The probe subprocess runs WITHOUT
// holding vm.mu.
func (vm *voiceManager) engineOK() bool {
	vm.mu.Lock()
	if vm.engineChecked {
		ready := vm.engineReady
		vm.mu.Unlock()
		return ready
	}
	vm.mu.Unlock()
	ok := exec.Command(vm.pythonBin, "-c", "import kokoro_onnx, soundfile").Run() == nil
	vm.mu.Lock()
	vm.engineChecked, vm.engineReady = true, ok
	vm.mu.Unlock()
	return ok
}

func fileBig(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 1_000_000
}

// Download runs scripts/download_voices.py once, capturing its progress lines so
// the UI can show them. Idempotent: a call while a download is in flight is a no-op.
func (vm *voiceManager) Download() {
	vm.mu.Lock()
	if vm.dlState == "downloading" {
		vm.mu.Unlock()
		return
	}
	vm.dlState, vm.dlLog, vm.dlErr = "downloading", "starting...", ""
	vm.mu.Unlock()

	go func() {
		script := filepath.Join(vm.repoRoot, "scripts", "download_voices.py")
		cmd := exec.Command(vm.pythonBin, script) // FIXED path, no request-derived args
		cmd.Dir = vm.repoRoot
		cmd.Stderr = os.Stderr // engine/pip errors go to the daemon log
		out, err := cmd.StdoutPipe()
		if err != nil {
			vm.finishDownload("error", "", "stdout pipe: "+err.Error())
			return
		}
		if err := cmd.Start(); err != nil {
			vm.finishDownload("error", "", "could not start "+vm.pythonBin+" (is Python installed / FLEETCHAT_PYTHON set?): "+err.Error())
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 8*1024), 256*1024)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				vm.setDlLog(line)
			}
		}
		werr := cmd.Wait()
		if werr == nil {
			// The downloader just (re)installed the engine -- invalidate the cached
			// engine probe BEFORE re-checking Installed() below, or a primed-false
			// cache (the weights-present-first state) makes this read stale-false,
			// wrongly report "weights missing", and Installed() stays false until a
			// restart -- the circular bug the review found (P2 #1).
			vm.mu.Lock()
			vm.engineChecked = false
			vm.mu.Unlock()
		}
		switch {
		case werr != nil:
			vm.finishDownload("error", "", "downloader exited: "+werr.Error())
		case vm.Installed():
			vm.finishDownload("done", "High-quality voices installed.", "")
		default:
			vm.finishDownload("error", "", "downloader finished but high-quality voices still aren't usable (engine or weights missing) -- see the daemon log")
		}
	}()
}

func (vm *voiceManager) setDlLog(line string) {
	vm.mu.Lock()
	vm.dlLog = line
	vm.mu.Unlock()
}

func (vm *voiceManager) finishDownload(state, logLine, errMsg string) {
	vm.mu.Lock()
	vm.dlState = state
	if logLine != "" {
		vm.dlLog = logLine
	}
	vm.dlErr = errMsg
	vm.mu.Unlock()
	if errMsg != "" {
		log.Printf("[voices] download error: %s", errMsg)
	} else {
		log.Printf("[voices] download %s", state)
	}
}

// DownloadStatus returns the current download state, last progress line, and error.
func (vm *voiceManager) DownloadStatus() (state, logLine, errMsg string) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.dlState, vm.dlLog, vm.dlErr
}

// StartSpeaker spawns agents/speaker.py (if not already running and the weights
// are installed). The speaker heartbeats /control/tts, which the daemon already
// tracks, so the browser voices step aside on their own.
func (vm *voiceManager) StartSpeaker() error {
	// Installed() (weights + engine) locks vm.mu internally, so check it BEFORE we
	// take our own lock -- otherwise engineOK() would deadlock on the re-entrant Lock.
	if !vm.Installed() {
		return fmt.Errorf("high-quality voices not fully installed yet -- download them first")
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.speakerOn && vm.speaker != nil && vm.speaker.Process != nil {
		return nil
	}
	script := filepath.Join(vm.repoRoot, "agents", "speaker.py")
	cmd := exec.Command(vm.pythonBin, script) // FIXED path, no request-derived args
	cmd.Dir = vm.repoRoot
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start speaker: %w", err)
	}
	vm.speaker, vm.speakerOn = cmd, true
	log.Printf("[voices] speaker started (pid %d)", cmd.Process.Pid)
	go func(c *exec.Cmd) { // reaper: clear the flag when it exits on its own
		_ = c.Wait()
		vm.mu.Lock()
		if vm.speaker == c {
			vm.speaker, vm.speakerOn = nil, false
		}
		vm.mu.Unlock()
		log.Printf("[voices] speaker exited")
	}(cmd)
	return nil
}

// StopSpeaker kills the speaker process if running.
func (vm *voiceManager) StopSpeaker() {
	vm.mu.Lock()
	c := vm.speaker
	vm.speaker, vm.speakerOn = nil, false
	vm.mu.Unlock()
	if c != nil && c.Process != nil {
		_ = c.Process.Kill()
		log.Printf("[voices] speaker stopped")
	}
}

// SpeakerRunning reports whether the daemon-managed speaker is up.
func (vm *voiceManager) SpeakerRunning() bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.speakerOn
}
