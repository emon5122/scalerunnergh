package runnerdist

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	link, dir  bool
}

func tgz(t *testing.T, entries []entry) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tgz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.link {
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeSymlink, Linkname: e.body})
			continue
		}
		if e.dir {
			tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeDir, Mode: 0o755})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(e.body))})
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	f.Close()
	b, _ := os.ReadFile(p)
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func serve(t *testing.T, b []byte) (string, *int) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/runner.tar.gz", &hits
}

func TestEnsureVerifiesAndCaches(t *testing.T) {
	b := tgz(t, []entry{{name: "run.sh", body: "#!/bin/sh\n"}, {name: "bin/x", body: "x"}, {name: "bin/link", body: "x", link: true}})
	url, hits := serve(t, b)
	root := t.TempDir()
	dir, err := Ensure(context.Background(), root, "1.2.3", sum(b), url)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("run.sh missing or not executable: %v", err)
	}
	if _, err := Ensure(context.Background(), root, "1.2.3", sum(b), url); err != nil || *hits != 1 {
		t.Fatalf("second Ensure must use the cache: hits=%d err=%v", *hits, err)
	}
	job := filepath.Join(root, "job")
	if err := CopyTree(dir, job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(job, ".srg-ok")); !os.IsNotExist(err) {
		t.Error("marker must not be copied")
	}
	os.WriteFile(filepath.Join(job, "bin", "x"), []byte("tampered"), 0o755)
	if orig, _ := os.ReadFile(filepath.Join(dir, "bin", "x")); string(orig) != "x" {
		t.Error("a job copy must not share files with the release")
	}
}

func TestEnsureRejectsWrongSHA(t *testing.T) {
	b := tgz(t, []entry{{name: "run.sh", body: "evil"}})
	url, _ := serve(t, b)
	root := t.TempDir()
	_, err := Ensure(context.Background(), root, "1.2.3", strings.Repeat("0", 64), url)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256 mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dist", "1.2.3")); !os.IsNotExist(err) {
		t.Fatal("nothing may be unpacked from an unverified archive")
	}
}

func TestEnsureRejectsEscapes(t *testing.T) {
	for name, entries := range map[string][]entry{
		"dotdot":          {{name: "../evil", body: "x"}},
		"abs symlink":     {{name: "l", body: "/etc/passwd", link: true}},
		"link escapes":    {{name: "bin/l", body: "../../outside", link: true}},
		"chained links":   {{name: "d/l", body: "..", link: true}, {name: "d/x", body: "l/..", link: true}, {name: "d/x/evil", body: "x"}},
		"write via link":  {{name: "l", body: "bin", link: true}, {name: "l", body: "overwrite"}},
		"target via link": {{name: "d/l", body: "..", link: true}, {name: "d/x", body: "l/..", link: true}},
		"link via link":   {{name: "top/keep", body: "x"}, {name: "a/b/c", body: "../../top", link: true}, {name: "a/b/c/l", body: "../../../victim", link: true}},
	} {
		b := tgz(t, entries)
		url, _ := serve(t, b)
		root := t.TempDir()
		if _, err := Ensure(context.Background(), root, "1", sum(b), url); err == nil {
			t.Errorf("%s: archive must be rejected", name)
		}
		if _, err := os.Stat(filepath.Join(root, "dist", "evil")); err == nil {
			t.Errorf("%s: file written outside the target", name)
		}
	}
}

// The real actions/runner tarball is packed from ".", so its entries are
// "./", "./bin/", "./run.sh", with relative links inside externals/.
func TestEnsureAcceptsDotRootLayout(t *testing.T) {
	b := tgz(t, []entry{
		{name: "./", dir: true}, {name: "./bin/", dir: true}, {name: "./run.sh", body: "#!/bin/sh\n"},
		{name: "./externals/node20/bin/npm", body: "../lib/node_modules/npm/bin/npm-cli.js", link: true},
		{name: "./externals/node20/lib/node_modules/npm/bin/npm-cli.js", body: "js"},
	})
	url, _ := serve(t, b)
	dir, err := Ensure(context.Background(), t.TempDir(), "1", sum(b), url)
	if err != nil {
		t.Fatalf("the runner tarball layout must unpack: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "externals", "node20", "bin", "npm")); err != nil || string(got) != "js" {
		t.Fatalf("link listed before its target must work: %q %v", got, err)
	}
}

func TestEnsureAcceptsLinkToLink(t *testing.T) {
	b := tgz(t, []entry{{name: "bin/real", body: "x"}, {name: "bin/a", body: "real", link: true}, {name: "bin/b", body: "a", link: true}})
	url, _ := serve(t, b)
	dir, err := Ensure(context.Background(), t.TempDir(), "1", sum(b), url)
	if err != nil {
		t.Fatalf("a symlink to an unpacked symlink is legitimate: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "bin", "b")); err != nil || string(got) != "x" {
		t.Fatalf("link chain broken: %q %v", got, err)
	}
}

func TestEnsureZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("run.cmd")
	w.Write([]byte("@echo off\r\n"))
	w, _ = zw.Create("bin/Runner.Listener.exe")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	b, _ := os.ReadFile(p)
	dir, err := Ensure(context.Background(), t.TempDir(), "1", sum(b), "file://"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "Runner.Listener.exe")); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultURL(t *testing.T) {
	u, err := DefaultURL("2.338.2")
	if err != nil || !strings.HasPrefix(u, "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-") {
		t.Fatalf("%q %v", u, err)
	}
}
