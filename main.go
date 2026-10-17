// Command adbdav mounts an Android device's storage on macOS over adb,
// without any kernel extension.
//
// It serves the device as WebDAV on loopback and hands that URL to macOS's
// built-in WebDAV client:
//
//	Finder ──webdav──> 127.0.0.1:PORT (adbdav) ──adb──> device /sdcard
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/olragon/adbdav/internal/mnt"
	"github.com/olragon/adbdav/internal/session"
	"github.com/olragon/adbdav/internal/ui"
)

const usage = `adbdav — mount Android internal storage on macOS over adb (no kernel extension)

  adbdav menubar  [flags]   menu bar app: device, status, mount/unmount
  adbdav mount    [flags]   serve and mount; Ctrl-C unmounts and exits
  adbdav serve    [flags]   serve WebDAV only, print the URL
  adbdav unmount  [flags]   unmount a mount point left behind
  adbdav status   [flags]   report adb, devices and mount state

flags:
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("adbdav: ")

	// Launched from a .app bundle there are no arguments and no terminal, so
	// the menu bar is the only sensible default there. macOS passes -psn_… to
	// a bundled process, which is not a flag of ours.
	cmd := defaultCommand()
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "-psn_") {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	home, _ := os.UserHomeDir()
	var (
		cfg       session.Config
		cacheMS   int
		verbose   bool
		autoMount bool
	)
	flag.StringVar(&cfg.ADBBin, "adb", lookupADB(), "path to the adb binary")
	flag.StringVar(&cfg.Serial, "serial", "", "device serial (default: the only attached device)")
	flag.StringVar(&cfg.Root, "root", "/sdcard", "device path to expose as the filesystem root")
	flag.StringVar(&cfg.MountPoint, "at", filepath.Join(home, "Android"), "mount point")
	flag.IntVar(&cfg.Port, "port", 0, "loopback port (0 picks a free one)")
	flag.StringVar(&cfg.VolName, "name", "Android", "volume name shown in Finder")
	flag.BoolVar(&cfg.Writable, "write", false,
		"allow writing to the device (the default mount is read-only)")
	flag.BoolVar(&cfg.NoVerify, "no-verify", false,
		"skip re-hashing each upload on the device after it is written")
	flag.BoolVar(&cfg.Thumbnails, "thumbnails", false,
		"let Quick Look build previews (reads every image in full over adb)")
	flag.BoolVar(&cfg.KeepAppleJunk, "keep-apple-junk", false,
		"write Finder's .DS_Store and ._ sidecars through to the device")
	flag.IntVar(&cacheMS, "cache", 5000, "metadata cache TTL in ms (0 disables)")
	flag.BoolVar(&verbose, "v", false, "log every WebDAV request")
	flag.BoolVar(&autoMount, "auto", false, "menubar: mount as soon as the app starts")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()

	cfg.CacheTTL = time.Duration(cacheMS) * time.Millisecond
	if verbose {
		cfg.OnLog = func(method, path string, err error) {
			log.Printf("%-9s %-50s %v", method, path, err)
		}
	} else {
		cfg.OnLog = func(method, path string, err error) {
			if err != nil {
				log.Printf("%-9s %-50s %v", method, path, err)
			}
		}
	}

	var err error
	switch cmd {
	case "menubar", "bar":
		ui.Run(session.New(cfg), autoMount)
	case "mount":
		err = runMount(cfg)
	case "serve":
		err = runServe(cfg)
	case "unmount", "umount":
		if err = mnt.Unmount(cfg.MountPoint); err == nil {
			fmt.Printf("unmounted %s\n", cfg.MountPoint)
		}
	case "status":
		err = runStatus(cfg)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// defaultCommand picks what `adbdav` with no verb should do: inside an .app
// bundle that is the menu bar, on a terminal it is a status report.
func defaultCommand() string {
	exe, err := os.Executable()
	if err == nil && strings.Contains(exe, ".app/Contents/MacOS/") {
		return "menubar"
	}
	return "status"
}

// lookupADB finds adb on PATH, falling back to the Homebrew
// command-line-tools location, which is not symlinked into bin by default.
func lookupADB() string {
	if p, err := exec.LookPath("adb"); err == nil {
		return p
	}
	const brew = "/opt/homebrew/share/android-commandlinetools/platform-tools/adb"
	if _, err := os.Stat(brew); err == nil {
		return brew
	}
	return "adb"
}

func runServe(cfg session.Config) error {
	s := session.New(cfg)
	url, err := s.Serve(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("serving %s at %s\n", cfg.Root, url)
	fmt.Printf("mount it with:\n  mount_webdav -S -v %s %s %s\n", cfg.VolName, url, cfg.MountPoint)
	waitForSignal()
	s.Stop()
	return nil
}

func runMount(cfg session.Config) error {
	s := session.New(cfg)
	if err := s.Mount(context.Background()); err != nil {
		return err
	}
	st := s.Status(context.Background())
	mode := "read-only"
	if cfg.Writable {
		mode = "WRITABLE"
	}
	fmt.Printf("mounted %s %s at %s (%s)\n", st.Serial, cfg.Root, cfg.MountPoint, mode)
	fmt.Printf("serving %s — Ctrl-C to unmount\n", st.URL)
	waitForSignal()
	fmt.Println("\nunmounting…")
	s.Stop()
	return nil
}

func waitForSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func runStatus(cfg session.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := session.New(cfg).Status(ctx)

	fmt.Printf("adb binary    : %s\n", st.ADBBin)
	switch {
	case st.Err != nil:
		fmt.Printf("adb devices   : ERROR %v\n", st.Err)
	case len(st.Attached) == 0:
		fmt.Printf("adb devices   : none\n")
	default:
		fmt.Printf("adb devices   : %s\n", strings.Join(st.Attached, ", "))
	}
	if st.Model != "" {
		fmt.Printf("model         : %s\n", st.Model)
	}
	fmt.Printf("mount_webdav  : %v\n", mnt.Available())
	state := "not mounted"
	if st.Mounted {
		state = "MOUNTED"
	}
	mode := "read-only"
	if !st.ReadOnly {
		mode = "writable"
	}
	fmt.Printf("mount point   : %s (%s, %s)\n", st.MountPoint, state, mode)
	if st.TotalBytes > 0 {
		fmt.Printf("%-14s: %.1f GiB free of %.1f GiB\n", cfg.Root,
			float64(st.FreeBytes)/(1<<30), float64(st.TotalBytes)/(1<<30))
	}
	return nil
}
