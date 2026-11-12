// Package runnerdist downloads and unpacks a pinned actions/runner release
// for the native backend. The archive is verified by sha256 before anything
// is unpacked, and the runner never updates itself.
package runnerdist

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultURL returns the GitHub release URL of the runner archive for this OS/arch.
func DefaultURL(version string) (string, error) {
	var osName, ext string
	switch runtime.GOOS {
	case "linux":
		osName, ext = "linux", "tar.gz"
	case "windows":
		osName, ext = "win", "zip"
	case "darwin":
		osName, ext = "osx", "tar.gz"
	default:
		return "", fmt.Errorf("runnerdist: unsupported OS %s", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("runnerdist: unsupported arch %s", runtime.GOARCH)
	}
	return fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-%s-%s-%s.%s",
		version, osName, arch, version, ext), nil
}

// Ensure returns the directory holding the unpacked runner release
// (<root>/dist/<version>), downloading and verifying it when missing.
func Ensure(ctx context.Context, root, version, sha256Hex, url string) (string, error) {
	dir := filepath.Join(root, "dist", version)
	marker := filepath.Join(dir, ".srg-ok")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == sha256Hex {
		return dir, nil
	}
	if url == "" {
		var err error
		if url, err = DefaultURL(version); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		return "", err
	}
	archive := filepath.Join(root, "dist", "download-"+version+".tmp")
	defer os.Remove(archive)
	if err := download(ctx, url, archive, sha256Hex); err != nil {
		return "", err
	}
	tmp := dir + ".unpack"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	var err error
	if strings.HasSuffix(strings.ToLower(url), ".zip") {
		err = unzip(archive, tmp)
	} else {
		err = untarGz(archive, tmp)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("runnerdist: unpack: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, []byte(sha256Hex+"\n"), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func download(ctx context.Context, url, dest, wantSHA string) error {
	var src io.ReadCloser
	if strings.HasPrefix(url, "file://") {
		f, err := os.Open(strings.TrimPrefix(url, "file://"))
		if err != nil {
			return fmt.Errorf("runnerdist: %w", err)
		}
		src = f
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
		if err != nil {
			return fmt.Errorf("runnerdist: download: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("runnerdist: download %s: status %d", url, resp.StatusCode)
		}
		src = resp.Body
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("runnerdist: download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("runnerdist: sha256 mismatch for %s: got %s, pinned %s", url, got, wantSHA)
	}
	return nil
}

// safeJoin refuses archive entries that would land outside dest, both by
// their name and through symlinks already unpacked on the way to them.
func safeJoin(dest, name string) (string, error) {
	p := filepath.Join(dest, filepath.FromSlash(name))
	if !within(dest, p) {
		return "", fmt.Errorf("archive entry %q escapes the target dir", name)
	}
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return "", err
	}
	// Resolve the deepest existing ancestor of the entry.
	anc := filepath.Dir(p)
	for {
		real, err := filepath.EvalSymlinks(anc)
		if err == nil {
			if !within(realDest, real) {
				return "", fmt.Errorf("archive entry %q escapes the target dir through a symlink", name)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		anc = filepath.Dir(anc)
	}
	// Never write through an existing symlink at the entry itself.
	if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("archive entry %q would overwrite a symlink", name)
	}
	return p, nil
}

func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func untarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(p, tr, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("archive symlink %q is absolute", hdr.Name)
			}
			if _, err := safeJoin(dest, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return err
			}
		}
	}
}

func unzip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		p, err := safeJoin(dest, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode() & 0o777
		if mode == 0 {
			mode = 0o644
		}
		err = writeFile(p, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// CopyTree copies the unpacked release into a fresh per-job directory.
// It is a real copy (no hardlinks), so a job can never alter the shared release.
func CopyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == ".srg-ok" {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			return writeFile(target, in, info.Mode()&0o777)
		}
		return nil
	})
}
