// Package mnt drives macOS's native WebDAV client.
//
// /sbin/mount_webdav is part of the base system and needs no kernel
// extension — which is the whole reason this project exists. macFUSE would
// work too, but on Apple Silicon loading its kext needs a Recovery boot.
package mnt

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const mountWebdav = "/sbin/mount_webdav"

// Available reports whether this machine can mount WebDAV at all.
func Available() bool {
	_, err := os.Stat(mountWebdav)
	return err == nil
}

// IsMounted reports whether anything is mounted at point.
func IsMounted(point string) bool {
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " on "+point+" ") {
			return true
		}
	}
	return false
}

// Mount attaches url at point, creating the directory if needed.
//
// readOnly adds -o ro, which marks the volume read-only in the KERNEL. That
// is worth having on top of the server refusing writes: Finder then greys out
// editing and stops trying to drop .DS_Store on it, instead of discovering
// mid-drag that the volume will not take the file.
func Mount(url, point, volName string, readOnly bool) error {
	if !Available() {
		return fmt.Errorf("mnt: %s not found — macOS only", mountWebdav)
	}
	if IsMounted(point) {
		return fmt.Errorf("mnt: already mounted: %s", point)
	}
	if err := os.MkdirAll(point, 0o755); err != nil {
		return err
	}
	// -S suppresses the keychain/auth prompt; the server is loopback-only
	// and unauthenticated, so there is nothing to ask for.
	args := []string{"-S"}
	if readOnly {
		args = append(args, "-o", "ro")
	}
	args = append(args, "-v", volName, url, point)
	c := exec.Command(mountWebdav, args...)
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("mnt: mount_webdav: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Unmount detaches point, forcing it if the polite form fails.
func Unmount(point string) error {
	if !IsMounted(point) {
		return nil
	}
	if err := exec.Command("/sbin/umount", point).Run(); err == nil {
		return nil
	}
	out, err := exec.Command("/usr/sbin/diskutil", "unmount", "force", point).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mnt: unmount %s: %s", point, strings.TrimSpace(string(out)))
	}
	return nil
}
