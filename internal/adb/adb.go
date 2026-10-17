// Package adb is the device layer: it turns filesystem intentions into adb
// invocations and parses what the device says back. It knows nothing about
// HTTP or WebDAV.
//
// Everything that produces output goes through `adb exec-out`, never
// `adb shell`. exec-out allocates no pty, so bytes arrive verbatim; `adb shell`
// can translate LF to CRLF and would corrupt every binary file.
package adb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// Entry is one directory entry as the device reports it.
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Device is one adb target.
type Device struct {
	Bin    string // path to the adb binary
	Serial string // optional; empty means "the only device"
	Root   string // device path exposed as the filesystem root, e.g. /sdcard
}

// New returns a Device with sane defaults filled in.
func New(bin, serial, root string) *Device {
	if bin == "" {
		bin = "adb"
	}
	if root == "" {
		root = "/sdcard"
	}
	return &Device{Bin: bin, Serial: serial, Root: strings.TrimRight(root, "/")}
}

// Quote renders s as a single shell word safe for the device shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Resolve maps a slash-separated filesystem path to an absolute device path.
//
// Containment comes from path.Clean on an ALREADY-ABSOLUTE path: every ".."
// collapses against "/", so "/../../etc/hosts" cleans to "/etc/hosts" and
// lands under Root. The prefix check below can therefore not fire — it is
// kept so that a future change to the line above fails loudly instead of
// silently opening the whole device.
func (d *Device) Resolve(name string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if clean == "/" {
		return d.Root, nil
	}
	full := d.Root + clean
	if !strings.HasPrefix(full, d.Root+"/") {
		return "", fmt.Errorf("adb: path escapes root: %q", name)
	}
	return full, nil
}

func (d *Device) args(rest ...string) []string {
	var a []string
	if d.Serial != "" {
		a = append(a, "-s", d.Serial)
	}
	return append(a, rest...)
}

// rcMarker terminates every wrapped command's output.
//
// ⛔ `adb exec-out` ALWAYS exits 0, whatever the device command did, and it
// merges the device's stderr into stdout. (`adb shell` does propagate the
// status, but it allocates a pty and would corrupt binary reads.) So a
// failing mkdir/rm/mv is invisible unless the status is carried back in-band.
const rcMarker = "__ADBDAV_RC="

func wrapRC(cmd string) string {
	return "{ " + cmd + " ; } 2>&1; printf '" + rcMarker + "%d' \"$?\""
}

// parseRC splits a wrapped command's output from its exit status.
func parseRC(out []byte) (body []byte, rc int, ok bool) {
	i := bytes.LastIndex(out, []byte(rcMarker))
	if i < 0 {
		return out, 0, false
	}
	rc, err := strconv.Atoi(string(bytes.TrimSpace(out[i+len(rcMarker):])))
	if err != nil {
		return out[:i], 0, false
	}
	return out[:i], rc, true
}

// Error is a non-zero exit from a device command.
type Error struct {
	Cmd    string
	RC     int
	Output string
}

func (e *Error) Error() string {
	return fmt.Sprintf("adb: %s: exit %d: %s", e.Cmd, e.RC, e.Output)
}

// Run executes a shell command on the device and returns its stdout.
// A non-zero device exit status becomes an *Error.
func (d *Device) Run(ctx context.Context, cmd string) ([]byte, error) {
	c := exec.CommandContext(ctx, d.Bin, d.args("exec-out", wrapRC(cmd))...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("adb: %s: %s", cmd, msg)
	}
	body, rc, ok := parseRC(out.Bytes())
	if !ok {
		// No marker: adb never reached a device shell (offline, unauthorised).
		return body, fmt.Errorf("adb: %s: no response from device shell: %s",
			cmd, strings.TrimSpace(string(body)))
	}
	if rc != 0 {
		return body, &Error{Cmd: cmd, RC: rc, Output: strings.TrimSpace(string(body))}
	}
	return body, nil
}

// statFormat is parsed by parseStatLine. -L dereferences symlinks, which
// matters because /sdcard is itself a link to /storage/self/primary.
const statFormat = `'%F|%s|%Y|%n'`

func parseStatLine(line string) (Entry, bool) {
	parts := strings.SplitN(strings.TrimRight(line, "\r"), "|", 4)
	if len(parts) != 4 {
		return Entry{}, false
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	secs, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	return Entry{
		Name:    parts[3],
		Size:    size,
		ModTime: time.Unix(secs, 0),
		IsDir:   parts[0] == "directory",
	}, true
}

// Stat reports one entry. Name is the full device path.
func (d *Device) Stat(ctx context.Context, devPath string) (Entry, error) {
	out, err := d.Run(ctx, "stat -Lc "+statFormat+" "+Quote(devPath))
	if err != nil {
		// Every stat failure is "it is not there" as far as WebDAV cares;
		// returning os.ErrNotExist is what makes the handler answer 404.
		return Entry{}, os.ErrNotExist
	}
	e, ok := parseStatLine(strings.TrimSpace(string(out)))
	if !ok {
		return Entry{}, fmt.Errorf("adb: unparsable stat for %q: %q", devPath, out)
	}
	e.Name = path.Base(devPath)
	return e, nil
}

// listCmd builds the one-round-trip directory listing.
//
// ⛔ The -H is load-bearing. /sdcard is a SYMLINK to /storage/self/primary,
// and find does not descend into a symlinked argument — without -H,
// `-mindepth 1` matches nothing and the root directory reads as EMPTY while
// every file inside it is still readable by path. -H dereferences the
// argument only, so symlinks met during the walk cannot cause a loop.
func listCmd(devPath string) string {
	return "find -H " + Quote(devPath) +
		" -maxdepth 1 -mindepth 1 -exec stat -Lc " + statFormat + " {} +" + devNull
}

// List reports the direct children of a directory.
//
// `find -maxdepth 1 -mindepth 1 -exec stat {} +` costs one adb round trip for
// the whole directory; stat'ing each child separately costs one per file and
// makes Finder crawl.
func (d *Device) List(ctx context.Context, devPath string) ([]Entry, error) {
	out, err := d.Run(ctx, listCmd(devPath))
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, ok := parseStatLine(line)
		if !ok {
			continue // a broken symlink or a name containing a newline
		}
		e.Name = path.Base(e.Name)
		entries = append(entries, e)
	}
	return entries, nil
}

// devNull keeps a device error message out of a binary payload: exec-out
// merges stderr into stdout, so an unredirected failure would be served to
// the client as file content.
const devNull = " 2>/dev/null"

// Open streams a file's bytes starting at off. A length of -1 means "to EOF".
//
// toybox dd's byte-granular flags are unreliable, so the portable range read is
// `tail -c +N | head -c L`. tail counts from 1, hence off+1.
func (d *Device) Open(ctx context.Context, devPath string, off, length int64) (io.ReadCloser, error) {
	var cmd string
	switch {
	case off <= 0 && length < 0:
		cmd = "cat " + Quote(devPath) + devNull
	case length < 0:
		cmd = "tail -c +" + strconv.FormatInt(off+1, 10) + " " + Quote(devPath) + devNull
	case off <= 0:
		cmd = "head -c " + strconv.FormatInt(length, 10) + " " + Quote(devPath) + devNull
	default:
		cmd = "tail -c +" + strconv.FormatInt(off+1, 10) + " " + Quote(devPath) +
			" | head -c " + strconv.FormatInt(length, 10) + devNull
	}
	c := exec.CommandContext(ctx, d.Bin, d.args("exec-out", cmd)...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c.Stderr = io.Discard
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &procReader{cmd: c, out: stdout}, nil
}

type procReader struct {
	cmd *exec.Cmd
	out io.ReadCloser
}

func (p *procReader) Read(b []byte) (int, error) { return p.out.Read(b) }

// Close kills the stream rather than waiting for it. A seek mid-file abandons
// the remainder, and `cat` of a multi-GB file would otherwise block Close.
func (p *procReader) Close() error {
	p.out.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	return nil
}

// Push uploads a local file, replacing the destination.
func (d *Device) Push(ctx context.Context, localPath, devPath string) error {
	c := exec.CommandContext(ctx, d.Bin, d.args("push", localPath, devPath)...)
	var errb bytes.Buffer
	c.Stderr = &errb
	c.Stdout = io.Discard
	if err := c.Run(); err != nil {
		return fmt.Errorf("adb push %s: %s", devPath, strings.TrimSpace(errb.String()))
	}
	return nil
}

func (d *Device) Mkdir(ctx context.Context, devPath string) error {
	_, err := d.Run(ctx, "mkdir "+Quote(devPath))
	return err
}

// RemoveAll deletes a path recursively.
//
// ⛔ It refuses the root itself. A WebDAV DELETE on the volume root is one
// mis-drag away in Finder, and `rm -rf /sdcard` is not recoverable.
func (d *Device) RemoveAll(ctx context.Context, devPath string) error {
	if path.Clean(devPath) == path.Clean(d.Root) {
		return fmt.Errorf("adb: refusing to delete the volume root %q", d.Root)
	}
	_, err := d.Run(ctx, "rm -rf "+Quote(devPath))
	return err
}

// Sha256 hashes a file on the device. The phone reads its own flash, so
// nothing is transferred: 80 MB hashes in ~0.17 s.
func (d *Device) Sha256(ctx context.Context, devPath string) (string, error) {
	out, err := d.Run(ctx, "sha256sum "+Quote(devPath))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", fmt.Errorf("adb: unparsable sha256sum for %q: %q", devPath, out)
	}
	return f[0], nil
}

func (d *Device) Rename(ctx context.Context, from, to string) error {
	_, err := d.Run(ctx, "mv -f "+Quote(from)+" "+Quote(to))
	return err
}

func (d *Device) Copy(ctx context.Context, from, to string) error {
	_, err := d.Run(ctx, "cp -a "+Quote(from)+" "+Quote(to))
	return err
}

// Touch sets a file's mtime; used to honour a WebDAV PROPPATCH.
func (d *Device) Touch(ctx context.Context, devPath string, t time.Time) error {
	_, err := d.Run(ctx, "touch -d "+Quote(t.UTC().Format("2006-01-02T15:04:05"))+" "+Quote(devPath))
	return err
}

// FreeBytes reports free space under Root, for the WebDAV quota properties.
// `df -B1` is not supported by toybox; -k is.
func (d *Device) FreeBytes(ctx context.Context) (free, total int64, err error) {
	out, err := d.Run(ctx, "df -k "+Quote(d.Root)+" | tail -1")
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(out))
	if len(f) < 4 {
		return 0, 0, fmt.Errorf("adb: unparsable df: %q", out)
	}
	totalK, _ := strconv.ParseInt(f[1], 10, 64)
	freeK, _ := strconv.ParseInt(f[3], 10, 64)
	return freeK * 1024, totalK * 1024, nil
}

// Prop reads an Android system property, e.g. ro.product.model.
func (d *Device) Prop(ctx context.Context, name string) string {
	out, err := d.Run(ctx, "getprop "+Quote(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Serials lists attached devices in the `device` state.
func Serials(ctx context.Context, bin string) ([]string, error) {
	if bin == "" {
		bin = "adb"
	}
	out, err := exec.CommandContext(ctx, bin, "devices").Output()
	if err != nil {
		return nil, err
	}
	var serials []string
	for _, line := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "device" {
			serials = append(serials, f[0])
		}
	}
	return serials, nil
}
