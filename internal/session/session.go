// Package session owns the lifecycle: pick a device, serve it as WebDAV on
// loopback, mount it, and take it all down again.
//
// It exists so the CLI and the menu bar drive the same code. A second
// implementation of "start the server then mount it" would drift.
package session

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/olragon/adbdav/internal/adb"
	"github.com/olragon/adbdav/internal/adbfs"
	"github.com/olragon/adbdav/internal/mnt"
)

// Config is everything a session needs to run.
type Config struct {
	ADBBin     string
	Serial     string // empty means "the only attached device"
	Root       string // device path exposed as the root
	MountPoint string
	VolName    string
	Port       int // 0 picks a free loopback port
	// CacheTTL is how long a stat or a listing may be reused. Zero means no
	// caching at all: the fail-safe value, and what `-cache 0` must actually
	// do — substituting a default here made "0 disables" a lie, and made a
	// benchmark of the uncached path silently measure the cached one.
	CacheTTL time.Duration
	// Writable is deliberately phrased in the positive: the zero value of a
	// Config is therefore a READ-ONLY mount, which is the safe one.
	Writable      bool
	KeepAppleJunk bool
	Thumbnails    bool
	NoVerify      bool
	OnLog         func(method, path string, err error)
}

// Status is a snapshot for display. Every field is safe to read while the
// session is running.
type Status struct {
	ADBBin     string
	Serial     string
	Model      string
	Attached   []string
	Serving    bool
	URL        string
	Mounted    bool
	MountPoint string
	ReadOnly   bool
	FreeBytes  int64
	TotalBytes int64
	Err        error
}

// Session is a running (or idle) adbdav instance.
type Session struct {
	mu  sync.Mutex
	cfg Config
	dev *adb.Device
	fs  *adbfs.FS
	srv *http.Server
	url string
}

func New(cfg Config) *Session {
	if cfg.Root == "" {
		cfg.Root = "/sdcard"
	}
	if cfg.VolName == "" {
		cfg.VolName = "Android"
	}
	return &Session{cfg: cfg}
}

func (s *Session) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// pickDevice resolves which device to talk to. Caller holds the lock.
func (s *Session) pickDevice(ctx context.Context) (*adb.Device, error) {
	serials, err := adb.Serials(ctx, s.cfg.ADBBin)
	if err != nil {
		return nil, fmt.Errorf("cannot run %s: %w", s.cfg.ADBBin, err)
	}
	serial := s.cfg.Serial
	switch {
	case serial != "":
	case len(serials) == 0:
		return nil, fmt.Errorf("no device in `device` state — check the cable and the USB-debugging prompt")
	case len(serials) > 1:
		return nil, fmt.Errorf("%d devices attached (%s) — pick one with -serial",
			len(serials), strings.Join(serials, ", "))
	default:
		serial = serials[0]
	}
	return adb.New(s.cfg.ADBBin, serial, s.cfg.Root), nil
}

// Serve starts the loopback WebDAV server if it is not already up and
// returns its URL.
func (s *Session) Serve(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return s.url, nil
	}
	dev, err := s.pickDevice(ctx)
	if err != nil {
		return "", err
	}
	fs := adbfs.New(dev, s.cfg.CacheTTL)
	fs.ReadOnly = !s.cfg.Writable
	fs.KeepAppleJunk = s.cfg.KeepAppleJunk
	fs.Thumbnails = s.cfg.Thumbnails
	fs.Verify = !s.cfg.NoVerify

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.cfg.Port))
	if err != nil {
		return "", err
	}
	// ⚠️ Finder names the volume after the LAST PATH COMPONENT of the URL,
	// not after mount_webdav's -v. Served at the root the volume shows up as
	// "127.0.0.1"; served under /<VolName> it shows up as the volume name.
	prefix := "/" + s.cfg.VolName
	h := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			// Finder probes every volume for .DS_Store and ._ sidecars that
			// will never exist; those misses are not worth reporting.
			if err != nil && os.IsNotExist(err) && adbfs.IsAppleMetadata(r.URL.Path) {
				return
			}
			if s.cfg.OnLog != nil {
				s.cfg.OnLog(r.Method, r.URL.Path, err)
			}
		},
	}
	srv := &http.Server{Handler: h}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			if s.cfg.OnLog != nil {
				s.cfg.OnLog("SERVE", "", err)
			}
		}
	}()
	s.dev, s.fs, s.srv = dev, fs, srv
	s.url = fmt.Sprintf("http://%s%s", ln.Addr().String(), prefix)
	return s.url, nil
}

// Mount serves (if needed) and attaches the volume.
func (s *Session) Mount(ctx context.Context) error {
	url, err := s.Serve(ctx)
	if err != nil {
		return err
	}
	cfg := s.Config()
	return mnt.Mount(url, cfg.MountPoint, cfg.VolName, !cfg.Writable)
}

// Unmount detaches the volume but leaves the server running.
func (s *Session) Unmount() error { return mnt.Unmount(s.Config().MountPoint) }

// SetThumbnails turns Quick Look previews on or off live. Finder picks the
// change up on its next probe of the marker file; no remount is needed.
func (s *Session) SetThumbnails(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Thumbnails = on
	if s.fs != nil {
		s.fs.Thumbnails = on
	}
}

// SetWritable changes write protection.
//
// Unlike the other toggles this REMOUNTS when the volume is up: read-only is
// enforced in the kernel by `mount -o ro`, and no server-side change can lift
// a flag the kernel is holding.
func (s *Session) SetWritable(ctx context.Context, on bool) error {
	s.mu.Lock()
	s.cfg.Writable = on
	if s.fs != nil {
		s.fs.ReadOnly = !on
	}
	point := s.cfg.MountPoint
	s.mu.Unlock()

	if !mnt.IsMounted(point) {
		return nil
	}
	if err := s.Unmount(); err != nil {
		return err
	}
	return s.Mount(ctx)
}

// Stop unmounts and shuts the server down. It is safe to call twice.
func (s *Session) Stop() {
	_ = s.Unmount()
	s.mu.Lock()
	srv := s.srv
	s.srv, s.fs, s.url = nil, nil, ""
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// Status reports what is currently true. It never blocks on a missing device
// for longer than ctx allows.
func (s *Session) Status(ctx context.Context) Status {
	s.mu.Lock()
	cfg, dev, srv, url := s.cfg, s.dev, s.srv, s.url
	s.mu.Unlock()

	st := Status{
		ADBBin:     cfg.ADBBin,
		MountPoint: cfg.MountPoint,
		ReadOnly:   !cfg.Writable,
		Serving:    srv != nil,
		URL:        url,
		Mounted:    mnt.IsMounted(cfg.MountPoint),
	}
	serials, err := adb.Serials(ctx, cfg.ADBBin)
	if err != nil {
		st.Err = err
		return st
	}
	st.Attached = serials
	if dev == nil {
		if cfg.Serial != "" {
			dev = adb.New(cfg.ADBBin, cfg.Serial, cfg.Root)
		} else if len(serials) == 1 {
			dev = adb.New(cfg.ADBBin, serials[0], cfg.Root)
		}
	}
	if dev == nil {
		return st
	}
	st.Serial = dev.Serial
	st.Model = dev.Prop(ctx, "ro.product.model")
	st.FreeBytes, st.TotalBytes, _ = dev.FreeBytes(ctx)
	return st
}
