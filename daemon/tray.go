package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"os/exec"
	"runtime"

	"github.com/gogpu/systray"
)

// runTray owns the main goroutine (a documented constraint of this library --
// its Run() pumps the platform's native event loop, which on Windows/macOS
// must be the main thread). Everything else -- the HTTP/WS server -- has to
// run in a goroutine started BEFORE this is called, never the other way
// around.
func runTray(bs *boardServer, reg *Registry) {
	iconLight := generateIcon(color.RGBA{R: 224, G: 122, B: 46, A: 255}) // matches FleetChat's own accent
	iconDark := generateIcon(color.RGBA{R: 110, G: 168, B: 224, A: 255})

	tray := systray.New()

	// "Shut down board" and "Start board" are two sides of ONE toggle, so the menu
	// shows only the one that applies to the current state -- never both. The menu
	// is rebuilt and re-applied via SetMenu on each toggle: win32Tray.SetMenu
	// destroys the old native HMENU and builds a fresh one, and a menu-item click
	// fires only AFTER the popup closes (TrackPopupMenu returns the selection, then
	// we dispatch), so calling SetMenu from inside a click handler safely swaps what
	// the NEXT right-click shows. buildMenu and refresh reference each other, so
	// buildMenu is declared before it's assigned.
	var buildMenu func() *systray.Menu
	refresh := func() { tray.SetMenu(buildMenu()) }
	buildMenu = func() *systray.Menu {
		menu := systray.NewMenu()
		menu.Add("Open board", func() { openBrowser("http://127.0.0.1:" + daemonPort) })
		menu.AddSeparator()
		menu.Add("Restart all agents", func() {
			if !bs.Running() {
				tray.ShowNotification("FleetChat", "Board is off -- Start board first.")
				return
			}
			n := reg.RestartAll()
			log.Printf("[tray] restarted %d agent(s)", n)
			tray.ShowNotification("FleetChat", fmt.Sprintf("Restarted %d agent(s)", n))
		})
		menu.Add("Restart board (reload binary)", func() {
			// Re-exec the whole PROCESS -- the only way to pick up a rebuilt daemon.exe
			// or recover a wedged server. Distinct from "Restart all agents" (recycles
			// only the agent subprocesses) and from Shut down/Start board (toggle the
			// board IN-process). Launch-then-exit so the port only ever hands off to a
			// process confirmed to exist; main.go's bind-retry absorbs the overlap.
			if !confirm("FleetChat — Restart board?",
				"Relaunch the whole board process (picks up a rebuilt binary)? Every agent is stopped and relaunched; any in-progress task is interrupted for a few seconds.") {
				return
			}
			exe, err := os.Executable()
			if err != nil {
				log.Printf("[tray] restart board failed: couldn't resolve own executable: %s", err)
				tray.ShowNotification("FleetChat", "Restart failed -- see log")
				return
			}
			wd, _ := os.Getwd() // best-effort; "" == inherit cwd if this fails
			cmd := exec.Command(exe)
			cmd.Dir = wd
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Start(); err != nil {
				log.Printf("[tray] restart board failed: couldn't start new process: %s", err)
				tray.ShowNotification("FleetChat", "Restart failed -- see log")
				return
			}
			log.Printf("[tray] new daemon process started (pid %d) -- handing off and exiting", cmd.Process.Pid)
			bs.Stop() // clean stop of THIS process's board (closes server + kills agents) before we hand off + exit
			tray.Remove()
			os.Exit(0)
		})
		menu.AddSeparator()
		// The one state-appropriate board toggle -- only ever ONE of these shows.
		if bs.Running() {
			menu.Add("Shut down board", func() {
				// Stop the board (server + agents) but LEAVE the app running -- the
				// whole point of the split. Reversible: history on disk survives and
				// the menu then offers "Start board". Light confirm (it interrupts
				// in-flight work); no dire warning because it doesn't quit the app.
				if !confirm("FleetChat — Shut down board?",
					"Stop the board? The web UI goes offline and the agents stop until you Start the board again. Your chat history is saved. This does NOT quit the app -- the tray stays.") {
					return
				}
				bs.Stop()
				tray.ShowNotification("FleetChat", "Board stopped. Open the tray and click 'Start board' to bring it back.")
				refresh() // swap the menu to show "Start board"
			})
		} else {
			menu.Add("Start board", func() {
				// Bring the board back after a "Shut down board" -- re-binds the port,
				// re-serves, re-spawns the crew.
				if err := bs.Start(); err != nil {
					log.Printf("[tray] start board failed: %s", err)
					tray.ShowNotification("FleetChat", "Start failed -- see log (is the port still free?)")
					return
				}
				tray.ShowNotification("FleetChat", "Board started.")
				refresh() // swap the menu to show "Shut down board"
			})
		}
		menu.AddSeparator()
		menu.Add("Exit application", func() {
			// Quit EVERYTHING -- board, agents, tray, process. The full quit, distinct
			// from "Shut down board" (which keeps the app alive). Real cleanup via
			// bs.Stop (kills every agent) -- os.Exit alone would orphan them.
			if !confirm("FleetChat — Exit application?",
				"Quit FleetChat entirely? The board, every agent, AND the tray icon all close. You'll need to relaunch to get it back.") {
				return
			}
			log.Println("[tray] exit application -- stopping board + agents, then quitting")
			bs.Stop()
			tray.Remove()
			os.Exit(0)
		})
		return menu
	}

	tray.SetIcon(iconLight).
		SetDarkModeIcon(iconDark).
		SetTooltip("FleetChat daemon")
	refresh() // install the initial menu with the state-appropriate toggle
	// Rebuild the menu to CURRENT state on every right-click, so a board stop/start
	// done OUT OF BAND (the /control/board API, or an unexpected serve-death ->
	// markStopped) shows the right item the next time the tray opens -- not only
	// after a tray-initiated toggle. Thread-safe: the lib fires OnRightClick on the
	// message-loop thread BEFORE it shows the popup (verified in platform_windows.go).
	// Covers the normal wmRButtonUp open path; the rare wmContextMenu fallback doesn't
	// fire OnRightClick, but the in-handler refresh + idempotent Stop/Start self-heal
	// that case in one click.
	tray.OnRightClick(refresh)
	tray.Show()

	log.Println("[tray] ready")
	if err := tray.Run(); err != nil {
		log.Printf("[tray] Run error: %s", err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[tray] couldn't open browser: %s", err)
	}
}

// generateIcon: same technique as the library's own example -- a small
// solid square with a white border, generated at runtime. No asset file
// needed; good enough to prove the tray mechanism works at all.
func generateIcon(c color.RGBA) []byte {
	const size = 22
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if x == 0 || x == size-1 || y == 0 || y == size-1 {
				img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			} else {
				img.SetRGBA(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
