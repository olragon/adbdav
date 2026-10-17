// Package ui is the macOS menu bar front end: it shows which device is
// attached, whether the volume is mounted, and drives the session's lifecycle.
package ui

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/getlantern/systray"

	"github.com/olragon/adbdav/internal/session"
)

// icon.png is a black-on-transparent template image; macOS recolours it for
// light, dark and highlighted menu bars. Regenerate with: go run ./internal/ui/gen
//
//go:embed icon.png
var iconPNG []byte

// Run shows the menu bar item and blocks until the user quits.
// It must be called from the main goroutine — systray owns the main run loop.
func Run(s *session.Session, autoMount bool) {
	m := &menu{sess: s, autoMount: autoMount}
	systray.Run(m.onReady, m.onExit)
}

type menu struct {
	sess      *session.Session
	autoMount bool

	device     *systray.MenuItem
	storage    *systray.MenuItem
	state      *systray.MenuItem
	mount      *systray.MenuItem
	unmount    *systray.MenuItem
	reveal     *systray.MenuItem
	readOnly   *systray.MenuItem
	thumbnails *systray.MenuItem
	refresh    *systray.MenuItem
	quit       *systray.MenuItem
}

func (m *menu) onReady() {
	systray.SetTemplateIcon(iconPNG, iconPNG)
	systray.SetTooltip("adbdav — Android over adb")

	m.device = systray.AddMenuItem("Device: …", "The attached Android device")
	m.storage = systray.AddMenuItem("Storage: …", "Free space on the device")
	m.state = systray.AddMenuItem("Status: …", "Mount state")
	m.device.Disable()
	m.storage.Disable()
	m.state.Disable()

	systray.AddSeparator()
	m.mount = systray.AddMenuItem("Mount", "Serve the device and mount it")
	m.unmount = systray.AddMenuItem("Unmount", "Detach the volume")
	m.reveal = systray.AddMenuItem("Open in Finder", "Reveal the mount point")

	systray.AddSeparator()
	m.readOnly = systray.AddMenuItemCheckbox("Read-only",
		"Protect the device. Turning this off remounts the volume.",
		!m.sess.Config().Writable)
	m.thumbnails = systray.AddMenuItemCheckbox("Thumbnails",
		"Let Finder build previews — reads every image in full over adb",
		m.sess.Config().Thumbnails)
	m.refresh = systray.AddMenuItem("Refresh", "Re-read device state now")

	systray.AddSeparator()
	m.quit = systray.AddMenuItem("Quit adbdav", "Unmount and exit")

	go m.loop()
}

// loop is the single owner of every menu interaction and of the periodic
// refresh, so no two goroutines can drive the session at once.
func (m *menu) loop() {
	if m.autoMount {
		m.do(func(ctx context.Context) error { return m.sess.Mount(ctx) })
	}
	m.update()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			m.update()
		case <-m.refresh.ClickedCh:
			m.update()
		case <-m.mount.ClickedCh:
			m.do(func(ctx context.Context) error { return m.sess.Mount(ctx) })
			m.update()
		case <-m.unmount.ClickedCh:
			m.do(func(context.Context) error { return m.sess.Unmount() })
			m.update()
		case <-m.reveal.ClickedCh:
			_ = exec.Command("/usr/bin/open", m.sess.Config().MountPoint).Run()
		case <-m.readOnly.ClickedCh:
			ro := !m.readOnly.Checked()
			m.do(func(ctx context.Context) error { return m.sess.SetWritable(ctx, !ro) })
			if ro {
				m.readOnly.Check()
			} else {
				m.readOnly.Uncheck()
			}
			m.update()
		case <-m.thumbnails.ClickedCh:
			on := !m.thumbnails.Checked()
			m.sess.SetThumbnails(on)
			if on {
				m.thumbnails.Check()
			} else {
				m.thumbnails.Uncheck()
			}
		case <-m.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// do runs one lifecycle action and surfaces a failure in the status line,
// where the user can actually see it — a menu bar app has no stderr.
func (m *menu) do(f func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f(ctx); err != nil {
		m.state.SetTitle("Status: " + truncate(err.Error(), 60))
	}
}

func (m *menu) update() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := m.sess.Status(ctx)

	switch {
	case st.Err != nil:
		m.device.SetTitle("Device: adb error")
	case len(st.Attached) == 0:
		m.device.SetTitle("Device: none attached")
	case st.Model != "":
		m.device.SetTitle(fmt.Sprintf("Device: %s (%s)", st.Model, st.Serial))
	default:
		m.device.SetTitle("Device: " + st.Serial)
	}

	if st.TotalBytes > 0 {
		m.storage.SetTitle(fmt.Sprintf("Storage: %.1f GB free of %.0f GB",
			float64(st.FreeBytes)/1e9, float64(st.TotalBytes)/1e9))
	} else {
		m.storage.SetTitle("Storage: unknown")
	}

	switch {
	case st.Mounted:
		mode := "read-only"
		if !st.ReadOnly {
			mode = "writable"
		}
		at := tilde(st.MountPoint)
		m.state.SetTitle("Status: mounted " + mode + " at " + at)
		systray.SetTooltip("adbdav — mounted " + mode + " at " + at)
	case st.Serving:
		m.state.SetTitle("Status: serving " + st.URL)
		systray.SetTooltip("adbdav — serving, not mounted")
	default:
		m.state.SetTitle("Status: idle")
		systray.SetTooltip("adbdav — idle")
	}

	canMount := !st.Mounted && (len(st.Attached) > 0 || st.Serial != "")
	setEnabled(m.mount, canMount)
	setEnabled(m.unmount, st.Mounted)
	setEnabled(m.reveal, st.Mounted)
}

func setEnabled(it *systray.MenuItem, on bool) {
	if on {
		it.Enable()
	} else {
		it.Disable()
	}
}

// onExit must leave no mounted volume behind: a mount pointing at a dead
// server wedges Finder until it is force-unmounted.
func (m *menu) onExit() { m.sess.Stop() }

// tilde shortens a path under the home directory. A menu row is narrow, the
// home prefix is noise, and it carries the account name into every
// screenshot anyone takes of this menu.
func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
