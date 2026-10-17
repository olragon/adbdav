// Package adbfs adapts an adb-attached device to the webdav.FileSystem
// contract. It depends on the device layer only through the Device interface
// below, so the protocol side can be tested against an in-memory fake.
package adbfs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"github.com/olragon/adbdav/internal/adb"
)

// File is webdav's file contract, re-exported so callers need not import it.
type File = webdav.File

// Device is the slice of *adb.Device this package needs. Declaring it here,
// at the consumer, keeps the dependency pointing at an abstraction.
type Device interface {
	Resolve(name string) (string, error)
	Stat(ctx context.Context, devPath string) (adb.Entry, error)
	List(ctx context.Context, devPath string) ([]adb.Entry, error)
	Open(ctx context.Context, devPath string, off, length int64) (io.ReadCloser, error)
	Push(ctx context.Context, localPath, devPath string) error
	Sha256(ctx context.Context, devPath string) (string, error)
	FreeBytes(ctx context.Context) (free, total int64, err error)
	Mkdir(ctx context.Context, devPath string) error
	RemoveAll(ctx context.Context, devPath string) error
	Rename(ctx context.Context, from, to string) error
}

// FS implements webdav.FileSystem over a device.
type FS struct {
	dev      Device
	ttl      time.Duration
	mu       sync.Mutex
	stats    map[string]statEntry
	lists    map[string]listEntry
	ReadOnly bool

	// KeepAppleJunk writes Finder's .DS_Store and ._ sidecars through to the
	// device. Off by default: they are of no use on a phone, and every one of
	// them costs an adb push.
	KeepAppleJunk bool

	// Thumbnails lets Quick Look render previews, which means Finder reads
	// every image in a folder in full over adb. Off by default — that is the
	// difference between a browsable volume and a stalled one.
	Thumbnails bool

	// Verify re-hashes each uploaded file on the device and compares it to
	// the bytes that were sent. On by default: the device hashes its own
	// flash, so an 80 MB file costs ~0.17 s and nothing is transferred.
	Verify bool
}

// spaceMargin is left free on the device rather than filling it exactly.
const spaceMargin = 16 << 20

// Marker files macOS looks for at a volume root. Serving them as empty files
// that exist only here — never on the device — is how this volume opts out of
// the two things that would otherwise read the whole phone over adb.
const (
	// Stops Spotlight indexing. Indexing half a terabyte over adb would
	// never finish.
	markerNeverIndex = ".metadata_never_index"
	// Stops Quick Look thumbnailing, which reads every image in full.
	markerNoThumbs  = ".ql_disablethumbnails"
	markerNoQLCache = ".ql_disablecache"
)

// synthetic reports whether name is one of the marker files this filesystem
// answers by itself, given the current settings.
func (f *FS) synthetic(name string) (string, bool) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	base := path.Base(clean)
	if path.Dir(clean) != "/" {
		return "", false // markers are only meaningful at the volume root
	}
	switch base {
	case markerNeverIndex:
		return base, true
	case markerNoThumbs, markerNoQLCache:
		return base, !f.Thumbnails
	}
	return "", false
}

type statEntry struct {
	e   adb.Entry
	err error
	at  time.Time
}

type listEntry struct {
	entries []adb.Entry
	at      time.Time
}

// New returns a filesystem. cacheTTL smooths over Finder's habit of issuing
// many PROPFINDs for the same directory in a burst; zero disables caching.
func New(dev Device, cacheTTL time.Duration) *FS {
	return &FS{
		dev:    dev,
		ttl:    cacheTTL,
		stats:  map[string]statEntry{},
		lists:  map[string]listEntry{},
		Verify: true,
		// Read-only is the default everywhere, so a caller that forgets to
		// set it cannot silently get a writable phone.
		ReadOnly: true,
	}
}

// invalidate drops the whole cache. Mutations are rare next to reads, so a
// blanket drop is cheaper to reason about than tracking parents.
func (f *FS) invalidate() {
	f.mu.Lock()
	clear(f.stats)
	clear(f.lists)
	f.mu.Unlock()
}

func (f *FS) stat(ctx context.Context, devPath string) (adb.Entry, error) {
	if f.ttl > 0 {
		f.mu.Lock()
		if c, ok := f.stats[devPath]; ok && time.Since(c.at) < f.ttl {
			f.mu.Unlock()
			return c.e, c.err
		}
		f.mu.Unlock()
	}
	e, err := f.dev.Stat(ctx, devPath)
	if f.ttl > 0 {
		f.mu.Lock()
		f.stats[devPath] = statEntry{e: e, err: err, at: time.Now()}
		f.mu.Unlock()
	}
	return e, err
}

func (f *FS) list(ctx context.Context, devPath string) ([]adb.Entry, error) {
	if f.ttl > 0 {
		f.mu.Lock()
		if c, ok := f.lists[devPath]; ok && time.Since(c.at) < f.ttl {
			f.mu.Unlock()
			return c.entries, nil
		}
		f.mu.Unlock()
	}
	entries, err := f.dev.List(ctx, devPath)
	if err != nil {
		return nil, err
	}
	if f.ttl > 0 {
		now := time.Now()
		f.mu.Lock()
		f.lists[devPath] = listEntry{entries: entries, at: now}
		// ⛔ Prime each child's stat from the listing we already hold.
		// webdav's walkFS re-Stats EVERY child after Readdir, so a 76-entry
		// directory costs 76 extra adb round trips — measured at 5.8 s for a
		// listing the device itself answers in 0.06 s. With this, those Stats
		// are cache hits and PROPFIND is one round trip.
		for _, c := range entries {
			f.stats[path.Join(devPath, c.Name)] = statEntry{e: c, at: now}
		}
		f.mu.Unlock()
	}
	return entries, nil
}

func (f *FS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	if f.ReadOnly {
		return os.ErrPermission
	}
	p, err := f.dev.Resolve(name)
	if err != nil {
		return err
	}
	if err := f.dev.Mkdir(ctx, p); err != nil {
		return err
	}
	f.invalidate()
	return nil
}

func (f *FS) RemoveAll(ctx context.Context, name string) error {
	if f.ReadOnly {
		return os.ErrPermission
	}
	if !f.KeepAppleJunk && IsAppleMetadata(name) {
		return nil
	}
	p, err := f.dev.Resolve(name)
	if err != nil {
		return err
	}
	if err := f.dev.RemoveAll(ctx, p); err != nil {
		return err
	}
	f.invalidate()
	return nil
}

func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	if f.ReadOnly {
		return os.ErrPermission
	}
	from, err := f.dev.Resolve(oldName)
	if err != nil {
		return err
	}
	to, err := f.dev.Resolve(newName)
	if err != nil {
		return err
	}
	if err := f.dev.Rename(ctx, from, to); err != nil {
		return err
	}
	f.invalidate()
	return nil
}

func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if base, ok := f.synthetic(name); ok {
		return FileInfo{adb.Entry{Name: base}}, nil
	}
	if !f.KeepAppleJunk && IsAppleMetadata(name) {
		return nil, os.ErrNotExist
	}
	p, err := f.dev.Resolve(name)
	if err != nil {
		return nil, err
	}
	e, err := f.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	return FileInfo{e}, nil
}

func (f *FS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (File, error) {
	p, err := f.dev.Resolve(name)
	if err != nil {
		return nil, err
	}
	if base, ok := f.synthetic(name); ok {
		return &memFile{info: FileInfo{adb.Entry{Name: base}}}, nil
	}
	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0
	if !f.KeepAppleJunk && IsAppleMetadata(name) {
		if writing {
			// Accept and discard: refusing makes Finder show an error sheet.
			return &memFile{info: FileInfo{adb.Entry{Name: path.Base(name)}}, discard: true}, nil
		}
		return nil, os.ErrNotExist
	}
	if writing && f.ReadOnly {
		return nil, os.ErrPermission
	}

	e, statErr := f.stat(ctx, p)
	if !writing {
		if statErr != nil {
			return nil, statErr
		}
		if e.IsDir {
			return f.openDir(ctx, p, e)
		}
		return &readFile{ctx: ctx, fs: f, devPath: p, info: FileInfo{e}}, nil
	}
	if statErr == nil && e.IsDir {
		return f.openDir(ctx, p, e)
	}
	// O_EXCL means "fail if it already exists".
	if statErr == nil && flag&os.O_EXCL != 0 {
		return nil, os.ErrExist
	}
	return f.openWrite(ctx, p, flag, e, statErr == nil)
}

func (f *FS) openDir(ctx context.Context, p string, e adb.Entry) (File, error) {
	entries, err := f.list(ctx, p)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, c := range entries {
		infos = append(infos, FileInfo{c})
	}
	return &dirFile{info: FileInfo{e}, children: infos}, nil
}

// openWrite stages the write in a local temp file and uploads it on Close.
// A partial `adb push` would leave a truncated file on the device, so the
// device is not touched until the client has finished writing.
func (f *FS) openWrite(ctx context.Context, p string, flag int, e adb.Entry, exists bool) (File, error) {
	tmp, err := os.CreateTemp("", "adbdav-*")
	if err != nil {
		return nil, err
	}
	w := &writeFile{ctx: ctx, fs: f, devPath: p, tmp: tmp, name: path.Base(p)}
	// A read-modify-write (no O_TRUNC) has to start from the current contents.
	if exists && flag&os.O_TRUNC == 0 && e.Size > 0 {
		rc, err := f.dev.Open(ctx, p, 0, -1)
		if err == nil {
			_, _ = io.Copy(tmp, rc)
			rc.Close()
			if flag&os.O_APPEND == 0 {
				_, _ = tmp.Seek(0, io.SeekStart)
			}
		}
	}
	return w, nil
}

// FileInfo adapts an adb.Entry to os.FileInfo.
type FileInfo struct{ E adb.Entry }

func (f FileInfo) Name() string { return f.E.Name }
func (f FileInfo) Size() int64  { return f.E.Size }
func (f FileInfo) Mode() os.FileMode {
	if f.E.IsDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (f FileInfo) ModTime() time.Time { return f.E.ModTime }

// ContentType is a hook webdav looks for on a FileInfo.
//
// ⛔ Without it, webdav answers getcontenttype by OPENING the file and reading
// 512 bytes whenever the extension is unknown — one adb round trip per file.
// Measured on 60 extension-less files: PROPFIND took 3.5 s, versus 0.3 s once
// the extension answers for them. macOS types files by extension anyway, so
// nothing is lost but the text/plain-vs-binary sniff on unnamed files.
func (f FileInfo) ContentType(context.Context) (string, error) {
	if f.E.IsDir {
		return "httpd/unix-directory", nil
	}
	if ct := mime.TypeByExtension(path.Ext(f.E.Name)); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}
func (f FileInfo) IsDir() bool { return f.E.IsDir }
func (f FileInfo) Sys() any    { return f.E }

// readFile streams bytes off the device, reopening the stream on a seek.
type readFile struct {
	ctx     context.Context
	fs      *FS
	devPath string
	info    FileInfo

	off   int64
	rc    io.ReadCloser
	rcOff int64 // where the open stream currently sits
}

func (r *readFile) Read(b []byte) (int, error) {
	if r.off >= r.info.Size() {
		return 0, io.EOF
	}
	if r.rc == nil || r.rcOff != r.off {
		if r.rc != nil {
			r.rc.Close()
			r.rc = nil
		}
		rc, err := r.fs.dev.Open(r.ctx, r.devPath, r.off, -1)
		if err != nil {
			return 0, err
		}
		r.rc, r.rcOff = rc, r.off
	}
	n, err := r.rc.Read(b)
	r.off += int64(n)
	r.rcOff += int64(n)
	return n, err
}

func (r *readFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = offset
	case io.SeekCurrent:
		r.off += offset
	case io.SeekEnd:
		r.off = r.info.Size() + offset
	}
	if r.off < 0 {
		r.off = 0
	}
	return r.off, nil
}

func (r *readFile) Close() error {
	if r.rc != nil {
		err := r.rc.Close()
		r.rc = nil
		return err
	}
	return nil
}

func (r *readFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (r *readFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (r *readFile) Stat() (os.FileInfo, error)         { return r.info, nil }

// writeFile buffers locally and pushes once, on Close.
type writeFile struct {
	ctx     context.Context
	fs      *FS
	devPath string
	name    string
	tmp     *os.File
	dirty   bool
	closed  bool
}

func (w *writeFile) Write(b []byte) (int, error) {
	w.dirty = true
	return w.tmp.Write(b)
}
func (w *writeFile) Read(b []byte) (int, error)          { return w.tmp.Read(b) }
func (w *writeFile) Seek(o int64, wh int) (int64, error) { return w.tmp.Seek(o, wh) }
func (w *writeFile) Readdir(int) ([]os.FileInfo, error)  { return nil, os.ErrInvalid }

func (w *writeFile) Stat() (os.FileInfo, error) {
	st, err := w.tmp.Stat()
	if err != nil {
		return nil, err
	}
	return FileInfo{adb.Entry{Name: w.name, Size: st.Size(), ModTime: st.ModTime()}}, nil
}

// Close publishes the staged file to the device.
//
// ⛔ It never pushes onto the destination path. A plain `adb push` is NOT
// atomic and NOT even truncating-safe: an interrupted push was measured to
// leave the destination GONE — the original file was destroyed and the new
// one never arrived. So the bytes go to a temp name in the same directory
// and are moved into place with rename(2), which is atomic. Until that
// rename, the original is untouched; if anything fails, it stays untouched.
func (w *writeFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	local := w.tmp.Name()
	defer os.Remove(local)
	if err := w.tmp.Close(); err != nil {
		return err
	}
	if !w.dirty {
		return nil
	}
	if err := w.fs.publish(w.ctx, local, w.devPath); err != nil {
		return err
	}
	w.fs.invalidate()
	return nil
}

// publish uploads local to devPath atomically, verifying it on the way.
func (f *FS) publish(ctx context.Context, local, devPath string) error {
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	size := st.Size()

	// Refuse before writing rather than half-filling the device.
	if free, _, err := f.dev.FreeBytes(ctx); err == nil && free > 0 && size > free-spaceMargin {
		return fmt.Errorf("adbfs: %s needs %d bytes, device has %d free", path.Base(devPath), size, free)
	}

	var want string
	if f.Verify {
		if want, err = hashFile(local); err != nil {
			return err
		}
	}

	staged := path.Join(path.Dir(devPath), tempName())
	if err := f.dev.Push(ctx, local, staged); err != nil {
		_ = f.dev.RemoveAll(ctx, staged)
		return err
	}
	if f.Verify {
		got, err := f.dev.Sha256(ctx, staged)
		if err != nil {
			_ = f.dev.RemoveAll(ctx, staged)
			return fmt.Errorf("adbfs: cannot verify %s: %w", path.Base(devPath), err)
		}
		if got != want {
			_ = f.dev.RemoveAll(ctx, staged)
			return fmt.Errorf("adbfs: %s corrupted in transfer (sent %s, device has %s) — original left untouched",
				path.Base(devPath), want[:12], got[:12])
		}
	}
	// Atomic from here: the destination is either the old file or the new one.
	if err := f.dev.Rename(ctx, staged, devPath); err != nil {
		_ = f.dev.RemoveAll(ctx, staged)
		return err
	}
	return nil
}

// tempName is hidden (dot-prefixed) so a half-written upload does not show
// up in Finder, and carries enough entropy for concurrent writers.
func tempName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf(".adbdav-tmp-%d", time.Now().UnixNano())
	}
	return ".adbdav-tmp-" + hex.EncodeToString(b[:])
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// memFile is an empty file that never touches the device: it backs the
// synthetic Spotlight/Quick Look markers and swallows Finder's sidecar writes.
type memFile struct {
	info    FileInfo
	discard bool
	off     int64
}

func (m *memFile) Read([]byte) (int, error) { return 0, io.EOF }
func (m *memFile) Write(b []byte) (int, error) {
	if !m.discard {
		return 0, os.ErrPermission
	}
	return len(b), nil
}
func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		m.off = offset
	}
	return m.off, nil
}
func (m *memFile) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (m *memFile) Stat() (os.FileInfo, error)         { return m.info, nil }
func (m *memFile) Close() error                       { return nil }

// dirFile serves a directory listing captured at open time.
type dirFile struct {
	info     FileInfo
	children []os.FileInfo
	pos      int
}

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		rest := d.children[d.pos:]
		d.pos = len(d.children)
		return rest, nil
	}
	if d.pos >= len(d.children) {
		return nil, io.EOF
	}
	end := min(d.pos+count, len(d.children))
	out := d.children[d.pos:end]
	d.pos = end
	return out, nil
}

func (d *dirFile) Stat() (os.FileInfo, error)     { return d.info, nil }
func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }

// IsAppleMetadata reports the junk Finder scatters over any volume it touches.
func IsAppleMetadata(name string) bool {
	b := path.Base(name)
	return b == ".DS_Store" || strings.HasPrefix(b, "._")
}
