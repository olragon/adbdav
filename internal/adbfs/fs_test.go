package adbfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"github.com/olragon/adbdav/internal/adb"
)

// fakeDev is an in-memory stand-in for a phone, so the protocol layer can be
// tested without one attached.
type fakeDev struct {
	mu    sync.Mutex
	root  string
	files map[string][]byte
	dirs  map[string]bool
	calls map[string]int
	order []string // every mutating op, in sequence

	free       int64
	corruptSha bool  // report a wrong hash, as a bad cable would
	failPushAt int64 // >0: fail a push of at least this many bytes
}

func newFake() *fakeDev {
	return &fakeDev{
		root:  "/sdcard",
		files: map[string][]byte{},
		dirs:  map[string]bool{"/sdcard": true},
		calls: map[string]int{},
		free:  1 << 40,
	}
}

func (f *fakeDev) Sha256(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return "", os.ErrNotExist
	}
	if f.corruptSha {
		return strings.Repeat("0", 64), nil
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeDev) FreeBytes(context.Context) (int64, int64, error) {
	return f.free, 1 << 40, nil
}

func (f *fakeDev) note(op string) {
	f.mu.Lock()
	f.calls[op]++
	f.mu.Unlock()
}

func (f *fakeDev) Resolve(name string) (string, error) {
	return adb.New("adb", "", f.root).Resolve(name)
}

func (f *fakeDev) Stat(_ context.Context, p string) (adb.Entry, error) {
	f.note("stat")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] {
		return adb.Entry{Name: path.Base(p), Size: 4096, ModTime: time.Unix(1700000000, 0), IsDir: true}, nil
	}
	if b, ok := f.files[p]; ok {
		return adb.Entry{Name: path.Base(p), Size: int64(len(b)), ModTime: time.Unix(1700000001, 0)}, nil
	}
	return adb.Entry{}, os.ErrNotExist
}

func (f *fakeDev) List(_ context.Context, dir string) ([]adb.Entry, error) {
	f.note("list")
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []adb.Entry
	seen := map[string]bool{}
	add := func(p string, isDir bool, size int64) {
		if path.Dir(p) != dir || p == dir || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, adb.Entry{Name: path.Base(p), Size: size, ModTime: time.Unix(1700000000, 0), IsDir: isDir})
	}
	for p := range f.dirs {
		add(p, true, 4096)
	}
	for p, b := range f.files {
		add(p, false, int64(len(b)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeDev) Open(_ context.Context, p string, off, length int64) (io.ReadCloser, error) {
	f.note("open")
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	if off > int64(len(b)) {
		off = int64(len(b))
	}
	b = b[off:]
	if length >= 0 && length < int64(len(b)) {
		b = b[:length]
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeDev) Push(_ context.Context, local, p string) error {
	f.note("push")
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPushAt > 0 && int64(len(b)) >= f.failPushAt {
		// A real interrupted push leaves the destination destroyed.
		delete(f.files, p)
		return errors.New("fake: push interrupted")
	}
	f.order = append(f.order, "push "+p)
	f.files[p] = b
	return nil
}

func (f *fakeDev) Mkdir(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] {
		return os.ErrExist
	}
	f.dirs[p] = true
	return nil
}

func (f *fakeDev) RemoveAll(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "rm "+p)
	delete(f.dirs, p)
	delete(f.files, p)
	for k := range f.files {
		if strings.HasPrefix(k, p+"/") {
			delete(f.files, k)
		}
	}
	return nil
}

func (f *fakeDev) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "rename "+from+" -> "+to)
	if b, ok := f.files[from]; ok {
		f.files[to] = b
		delete(f.files, from)
		return nil
	}
	if f.dirs[from] {
		delete(f.dirs, from)
		f.dirs[to] = true
		return nil
	}
	return os.ErrNotExist
}

func serverFor(t *testing.T, dev *fakeDev, ro bool) *httptest.Server {
	t.Helper()
	fs := New(dev, 0) // no caching in tests — assertions must see every write
	fs.ReadOnly = ro
	srv := httptest.NewServer(&webdav.Handler{FileSystem: fs, LockSystem: webdav.NewMemLS()})
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, p string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+p, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// macOS mounts read-only unless the server advertises DAV class 2 (locking).
func TestOptionsAdvertisesClass2(t *testing.T) {
	srv := serverFor(t, newFake(), false)
	res := do(t, srv, "OPTIONS", "/", nil, nil)
	defer res.Body.Close()
	dav := res.Header.Get("DAV")
	if !strings.Contains(dav, "1") || !strings.Contains(dav, "2") {
		t.Fatalf("DAV header = %q, want classes 1 and 2 (macOS mounts read-only otherwise)", dav)
	}
}

func TestPropfindListsChildren(t *testing.T) {
	dev := newFake()
	dev.dirs["/sdcard/DCIM"] = true
	dev.files["/sdcard/notes.txt"] = []byte("hello")
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	for _, want := range []string{"DCIM", "notes.txt", "<D:collection"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("PROPFIND body missing %q\n%s", want, body)
		}
	}
	if !bytes.Contains(body, []byte("<D:getcontentlength>5<")) {
		t.Errorf("size of notes.txt not reported\n%s", body)
	}
}

func TestGetAndRange(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/a.bin"] = []byte("0123456789")
	srv := serverFor(t, dev, false)

	res := do(t, srv, "GET", "/a.bin", nil, nil)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "0123456789" {
		t.Fatalf("GET body = %q", body)
	}

	// Finder streams media with Range requests; a wrong offset here silently
	// corrupts every large file.
	res = do(t, srv, "GET", "/a.bin", nil, map[string]string{"Range": "bytes=3-6"})
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", res.StatusCode)
	}
	if string(body) != "3456" {
		t.Fatalf("range body = %q, want %q", body, "3456")
	}
}

func TestPutCreatesOnDevice(t *testing.T) {
	dev := newFake()
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/new.txt", strings.NewReader("written"), nil)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d", res.StatusCode)
	}
	if got := string(dev.files["/sdcard/new.txt"]); got != "written" {
		t.Fatalf("device holds %q, want %q", got, "written")
	}
	// The upload must be one push, not a push per write call.
	if n := dev.calls["push"]; n != 1 {
		t.Errorf("push called %d times, want 1", n)
	}
}

func TestPutBinaryRoundTrip(t *testing.T) {
	dev := newFake()
	srv := serverFor(t, dev, false)

	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	res := do(t, srv, "PUT", "/blob.bin", bytes.NewReader(payload), nil)
	res.Body.Close()

	res = do(t, srv, "GET", "/blob.bin", nil, nil)
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("binary round trip corrupted: %d bytes in, %d out", len(payload), len(got))
	}
}

func TestMkcolDeleteMove(t *testing.T) {
	dev := newFake()
	srv := serverFor(t, dev, false)

	res := do(t, srv, "MKCOL", "/newdir", nil, nil)
	res.Body.Close()
	if !dev.dirs["/sdcard/newdir"] {
		t.Fatal("MKCOL did not create the directory")
	}

	dev.files["/sdcard/x.txt"] = []byte("x")
	res = do(t, srv, "MOVE", "/x.txt", nil, map[string]string{"Destination": srv.URL + "/y.txt"})
	res.Body.Close()
	if _, ok := dev.files["/sdcard/y.txt"]; !ok {
		t.Fatal("MOVE did not rename")
	}

	res = do(t, srv, "DELETE", "/y.txt", nil, nil)
	res.Body.Close()
	if _, ok := dev.files["/sdcard/y.txt"]; ok {
		t.Fatal("DELETE did not remove")
	}
}

func TestReadOnlyRefusesWrites(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/a.txt"] = []byte("keep")
	srv := serverFor(t, dev, true)

	for _, tc := range []struct {
		method, path string
		body         io.Reader
	}{
		{"PUT", "/b.txt", strings.NewReader("no")},
		{"MKCOL", "/d", nil},
		{"DELETE", "/a.txt", nil},
	} {
		res := do(t, srv, tc.method, tc.path, tc.body, nil)
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Errorf("%s %s = %d, want a refusal", tc.method, tc.path, res.StatusCode)
		}
	}
	if string(dev.files["/sdcard/a.txt"]) != "keep" {
		t.Error("read-only mode let a write through")
	}
}

// A seek backwards must reopen the stream at the new offset, not continue
// reading from where the previous stream stood.
func TestReadFileSeek(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/s.bin"] = []byte("ABCDEFGHIJ")
	fs := New(dev, 0)
	f, err := fs.OpenFile(context.Background(), "/s.bin", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	buf := make([]byte, 3)
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != "ABC" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if _, err := f.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(f, buf); err != nil || string(buf) != "HIJ" {
		t.Fatalf("after seek = %q, %v", buf, err)
	}
	if pos, _ := f.Seek(-2, io.SeekEnd); pos != 8 {
		t.Fatalf("SeekEnd = %d, want 8", pos)
	}
}

// The cache must not serve a directory listing that a write has invalidated.
func TestCacheInvalidatedOnWrite(t *testing.T) {
	dev := newFake()
	fs := New(dev, 10*time.Second)
	fs.ReadOnly = false
	ctx := context.Background()

	if _, err := fs.OpenFile(ctx, "/", os.O_RDONLY, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "/fresh", 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := fs.OpenFile(ctx, "/", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	infos, _ := d.Readdir(0)
	for _, i := range infos {
		if i.Name() == "fresh" {
			return
		}
	}
	t.Fatal("new directory not visible — the cache was not invalidated")
}

func TestIsAppleMetadata(t *testing.T) {
	for _, n := range []string{"/x/.DS_Store", "/._thing", ".DS_Store"} {
		if !IsAppleMetadata(n) {
			t.Errorf("%q should be Apple metadata", n)
		}
	}
	for _, n := range []string{"/x/photo.jpg", "/notes.txt", "/.hidden"} {
		if IsAppleMetadata(n) {
			t.Errorf("%q should not be Apple metadata", n)
		}
	}
}

// The marker files must be answered here and never looked for on the device —
// Spotlight indexing or Quick Look thumbnailing over adb stalls the volume.
func TestSyntheticMarkers(t *testing.T) {
	dev := newFake()
	fs := New(dev, 0)
	ctx := context.Background()

	for _, name := range []string{"/.metadata_never_index", "/.ql_disablethumbnails", "/.ql_disablecache"} {
		fi, err := fs.Stat(ctx, name)
		if err != nil {
			t.Errorf("Stat(%q) = %v, want a file", name, err)
			continue
		}
		if fi.Size() != 0 {
			t.Errorf("Stat(%q).Size = %d, want 0", name, fi.Size())
		}
	}
	if dev.calls["stat"] != 0 {
		t.Errorf("markers cost %d device round trips, want 0", dev.calls["stat"])
	}

	// With thumbnails enabled the Quick Look markers must disappear, or the
	// setting could never be turned back on.
	fs.Thumbnails = true
	if _, err := fs.Stat(ctx, "/.ql_disablethumbnails"); err == nil {
		t.Error("ql marker still present with thumbnails enabled")
	}
	if _, err := fs.Stat(ctx, "/.metadata_never_index"); err != nil {
		t.Error("Spotlight marker must not depend on the thumbnail setting")
	}
}

// Markers are only meaningful at the volume root; a real file of the same
// name deeper in the tree must not be shadowed.
func TestSyntheticOnlyAtRoot(t *testing.T) {
	dev := newFake()
	dev.dirs["/sdcard/sub"] = true
	dev.files["/sdcard/sub/.metadata_never_index"] = []byte("real content")
	fs := New(dev, 0)

	fi, err := fs.Stat(context.Background(), "/sub/.metadata_never_index")
	if err != nil {
		t.Fatalf("nested file hidden by the marker: %v", err)
	}
	if fi.Size() != int64(len("real content")) {
		t.Errorf("size = %d, want the real file's size", fi.Size())
	}
}

// Finder scatters .DS_Store and ._ sidecars over every volume. Writing them
// to a phone is pointless and costs an adb push each.
func TestAppleJunkDiscarded(t *testing.T) {
	dev := newFake()
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/.DS_Store", strings.NewReader("junk"), nil)
	res.Body.Close()
	if res.StatusCode >= 400 {
		t.Errorf("PUT .DS_Store = %d; refusing makes Finder show an error", res.StatusCode)
	}
	if len(dev.files) != 0 {
		t.Errorf("junk reached the device: %v", dev.files)
	}
	if dev.calls["push"] != 0 {
		t.Errorf("junk cost %d pushes, want 0", dev.calls["push"])
	}
}

// A directory listing must prime each child's stat: webdav re-Stats every
// child after Readdir, which was 76 extra round trips and 5.8s per PROPFIND.
func TestListPrimesChildStats(t *testing.T) {
	dev := newFake()
	for _, n := range []string{"a", "b", "c", "d"} {
		dev.files["/sdcard/"+n] = []byte(n)
	}
	fs := New(dev, 10*time.Second)
	ctx := context.Background()

	if _, err := fs.OpenFile(ctx, "/", os.O_RDONLY, 0); err != nil {
		t.Fatal(err)
	}
	before := dev.calls["stat"]
	for _, n := range []string{"a", "b", "c", "d"} {
		if _, err := fs.Stat(ctx, "/"+n); err != nil {
			t.Fatalf("Stat(%q): %v", n, err)
		}
	}
	if extra := dev.calls["stat"] - before; extra != 0 {
		t.Errorf("%d device stats after a listing, want 0 — the cache was not primed", extra)
	}
}

// Without a ContentType hook, webdav sniffs an unknown extension by opening
// the file and reading 512 bytes — one device round trip per file. Measured
// at 3.5s for 60 extension-less files versus 0.3s with the hook.
func TestContentTypeNeedsNoDeviceRead(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/a.jpg"] = []byte("x")
	dev.files["/sdcard/notes"] = []byte("plain text, no extension")
	dev.files["/sdcard/cap.pcap"] = []byte("x")
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if !bytes.Contains(body, []byte("image/jpeg")) {
		t.Errorf("jpg not typed by extension\n%s", body)
	}
	if !bytes.Contains(body, []byte("application/octet-stream")) {
		t.Errorf("extension-less file not given a fallback type\n%s", body)
	}
	if n := dev.calls["open"]; n != 0 {
		t.Errorf("content typing cost %d device reads, want 0", n)
	}
}

// ⛔ A plain `adb push` onto the destination is not safe: an interrupted one
// was measured to DESTROY the original and deliver nothing. Writes must stage
// under a temp name and rename(2) into place.
func TestWriteIsAtomic(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/doc.txt"] = []byte("ORIGINAL")
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/doc.txt", strings.NewReader("REPLACEMENT"), nil)
	res.Body.Close()

	if got := string(dev.files["/sdcard/doc.txt"]); got != "REPLACEMENT" {
		t.Fatalf("file holds %q", got)
	}
	if len(dev.order) < 2 {
		t.Fatalf("expected a staged write, got %v", dev.order)
	}
	if !strings.HasPrefix(dev.order[0], "push /sdcard/.adbdav-tmp-") {
		t.Errorf("first op was %q — the push must go to a temp name, not the destination", dev.order[0])
	}
	last := dev.order[len(dev.order)-1]
	if !strings.HasSuffix(last, "-> /sdcard/doc.txt") || !strings.HasPrefix(last, "rename ") {
		t.Errorf("last op was %q — the file must be published by rename", last)
	}
	for p := range dev.files {
		if strings.Contains(p, ".adbdav-tmp-") {
			t.Errorf("temp file left behind: %s", p)
		}
	}
}

// If the transfer dies, the original must still be there afterwards.
func TestFailedWriteLeavesOriginalIntact(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/doc.txt"] = []byte("ORIGINAL")
	dev.failPushAt = 1
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/doc.txt", strings.NewReader("REPLACEMENT"), nil)
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Errorf("failed push reported as %d, want an error", res.StatusCode)
	}
	if got := string(dev.files["/sdcard/doc.txt"]); got != "ORIGINAL" {
		t.Fatalf("original destroyed by a failed write: %q", got)
	}
}

// A corrupted transfer must be detected and must not replace the original.
func TestCorruptedWriteIsRejected(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/doc.txt"] = []byte("ORIGINAL")
	dev.corruptSha = true
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/doc.txt", strings.NewReader("REPLACEMENT"), nil)
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Errorf("corrupt upload accepted with %d", res.StatusCode)
	}
	if got := string(dev.files["/sdcard/doc.txt"]); got != "ORIGINAL" {
		t.Fatalf("corrupt upload replaced the original: %q", got)
	}
	for p := range dev.files {
		if strings.Contains(p, ".adbdav-tmp-") {
			t.Errorf("corrupt staged file left behind: %s", p)
		}
	}
}

// Filling the device is refused before anything is written.
func TestRefusesWriteWithoutSpace(t *testing.T) {
	dev := newFake()
	dev.free = 1024
	srv := serverFor(t, dev, false)

	res := do(t, srv, "PUT", "/big.bin", bytes.NewReader(make([]byte, 1<<20)), nil)
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Errorf("oversized write accepted with %d", res.StatusCode)
	}
	if dev.calls["push"] != 0 {
		t.Error("pushed anyway — the space check must come first")
	}
}

// Read-only must be the DEFAULT, so a caller that forgets to set it cannot
// hand out a writable phone by omission.
func TestNewDefaultsToReadOnly(t *testing.T) {
	fs := New(newFake(), 0)
	if !fs.ReadOnly {
		t.Fatal("New() returned a writable filesystem; the safe state must be the default")
	}
	if err := fs.Mkdir(context.Background(), "/nope", 0o755); err == nil {
		t.Error("Mkdir succeeded on a default filesystem")
	}
}

// A zero Config must describe a read-only mount.
func TestZeroConfigIsReadOnly(t *testing.T) {
	dev := newFake()
	fs := New(dev, 0)
	fs.ReadOnly = !false // what session does with a zero Config: !cfg.Writable
	if !fs.ReadOnly {
		t.Fatal("zero Config produced a writable mount")
	}
}

// A TTL of zero must mean "ask the device every time". The whole test suite
// above relies on it — and so does `-cache 0`, which is the only way to
// measure the uncached path honestly.
func TestZeroTTLDoesNotCache(t *testing.T) {
	dev := newFake()
	dev.files["/sdcard/a.txt"] = []byte("x")

	fs := New(dev, 0)
	for i := 0; i < 3; i++ {
		if _, err := fs.Stat(context.Background(), "/a.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if got := dev.calls["stat"]; got != 3 {
		t.Fatalf("device stats = %d, want 3 — a zero TTL must not cache", got)
	}

	cached := New(dev, 10*time.Second)
	for i := 0; i < 3; i++ {
		if _, err := cached.Stat(context.Background(), "/a.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if got := dev.calls["stat"]; got != 4 {
		t.Fatalf("device stats = %d, want 4 — a positive TTL must cache", got)
	}
}
