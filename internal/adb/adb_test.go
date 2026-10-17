package adb

import (
	"strings"
	"testing"
	"time"
)

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"a;rm -rf /", "'a;rm -rf /'"},
		{"$(whoami)", "'$(whoami)'"},
		{"`id`", "'`id`'"},
	} {
		if got := Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	d := New("adb", "", "/sdcard")
	for _, tc := range []struct{ in, want string }{
		{"/", "/sdcard"},
		{"", "/sdcard"},
		{"/DCIM", "/sdcard/DCIM"},
		{"DCIM/Camera", "/sdcard/DCIM/Camera"},
		{"/DCIM/../Download", "/sdcard/Download"},
	} {
		got, err := d.Resolve(tc.in)
		if err != nil {
			t.Errorf("Resolve(%q) unexpected error %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The security property is CONTAINMENT, not rejection: a ".." walk is
// flattened against the root rather than refused, exactly as http.Dir does.
func TestResolveContainsEscapes(t *testing.T) {
	d := New("adb", "", "/sdcard")
	for _, name := range []string{
		"/../secret", "../secret", "/a/../../secret",
		"/../../etc/hosts", "/..", "....//....//etc",
	} {
		got, err := d.Resolve(name)
		if err != nil {
			continue // refusal is an acceptable outcome too
		}
		if got != "/sdcard" && !strings.HasPrefix(got, "/sdcard/") {
			t.Errorf("Resolve(%q) = %q — escaped the root", name, got)
		}
	}
}

// A non-default root must be honoured, including one with a trailing slash.
func TestResolveCustomRoot(t *testing.T) {
	d := New("adb", "", "/storage/emulated/0/")
	got, err := d.Resolve("/Download/x.bin")
	if err != nil || got != "/storage/emulated/0/Download/x.bin" {
		t.Errorf("Resolve = %q, %v", got, err)
	}
}

func TestParseStatLine(t *testing.T) {
	e, ok := parseStatLine("regular file|6169444|1780000000|/sdcard/DCIM/Camera/IMG.jpg")
	if !ok {
		t.Fatal("expected a parse")
	}
	if e.IsDir {
		t.Error("regular file parsed as a directory")
	}
	if e.Size != 6169444 {
		t.Errorf("size = %d", e.Size)
	}
	if !e.ModTime.Equal(time.Unix(1780000000, 0)) {
		t.Errorf("modtime = %v", e.ModTime)
	}

	d, ok := parseStatLine("directory|3452|1775541611|/sdcard/DCIM")
	if !ok || !d.IsDir {
		t.Error("directory not recognised")
	}

	// A name containing the separator must survive: SplitN caps at 4.
	n, ok := parseStatLine("regular file|10|1|/sdcard/we|ird.txt")
	if !ok || n.Name != "/sdcard/we|ird.txt" {
		t.Errorf("pipe in filename lost: %q", n.Name)
	}

	for _, bad := range []string{"", "garbage", "regular file|notanumber|1|/x", "regular file|1|notanumber|/x"} {
		if _, ok := parseStatLine(bad); ok {
			t.Errorf("parseStatLine(%q) accepted bad input", bad)
		}
	}
}

// Trailing CR must be stripped: some adb transports still deliver it.
func TestParseStatLineStripsCR(t *testing.T) {
	e, ok := parseStatLine("directory|4096|1|/sdcard/X\r")
	if !ok || e.Name != "/sdcard/X" {
		t.Errorf("CR not stripped: %q", e.Name)
	}
}

// exec-out always exits 0 and merges stderr into stdout, so the device's real
// status has to ride back inside the output. These two guard that contract.
func TestWrapRCRoundTrip(t *testing.T) {
	w := wrapRC("stat /nope")
	if !strings.Contains(w, "stat /nope") || !strings.Contains(w, rcMarker) {
		t.Fatalf("wrapRC lost the command or the marker: %q", w)
	}
	if !strings.Contains(w, "2>&1") {
		t.Error("wrapRC must capture stderr, or an error message is lost")
	}
}

func TestParseRC(t *testing.T) {
	body, rc, ok := parseRC([]byte("hello\n" + rcMarker + "0"))
	if !ok || rc != 0 || string(body) != "hello\n" {
		t.Errorf("ok=%v rc=%d body=%q", ok, rc, body)
	}

	body, rc, ok = parseRC([]byte("stat: no such file\n" + rcMarker + "1"))
	if !ok || rc != 1 || string(body) != "stat: no such file\n" {
		t.Errorf("failure case: ok=%v rc=%d body=%q", ok, rc, body)
	}

	// No marker means adb never reached a device shell — that must not be
	// mistaken for success.
	if _, _, ok := parseRC([]byte("device offline")); ok {
		t.Error("missing marker reported as ok")
	}

	// Output that itself contains the marker: the LAST one wins.
	body, rc, ok = parseRC([]byte(rcMarker + "9 in a file\n" + rcMarker + "0"))
	if !ok || rc != 0 || !strings.Contains(string(body), "in a file") {
		t.Errorf("LastIndex not used: ok=%v rc=%d body=%q", ok, rc, body)
	}
}

func TestOpenRedirectsStderr(t *testing.T) {
	// A binary read must never be able to deliver a device error message as
	// file content.
	if !strings.HasPrefix(devNull, " 2>/dev/null") {
		t.Fatalf("devNull = %q", devNull)
	}
}

// /sdcard is a symlink; without -H the root listing silently comes back empty
// while individual files still resolve. Guard the flag.
func TestListCmdDereferencesSymlinkedRoot(t *testing.T) {
	c := listCmd("/sdcard")
	if !strings.Contains(c, "find -H ") {
		t.Fatalf("listCmd must pass -H, got: %s", c)
	}
	for _, want := range []string{"-maxdepth 1", "-mindepth 1", "stat -Lc", "'/sdcard'"} {
		if !strings.Contains(c, want) {
			t.Errorf("listCmd missing %q: %s", want, c)
		}
	}
}
