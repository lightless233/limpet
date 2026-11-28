package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func getDirs(t *testing.T, m *manager, rawQuery string) (int, dirListing) {
	t.Helper()
	target := "/api/dirs"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	w := httptest.NewRecorder()
	m.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	var out dirListing
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s invalid json: %v (%s)", target, err, w.Body.Bytes())
		}
	}
	return w.Code, out
}

func TestHandleListDirs(t *testing.T) {
	m := testManager(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, name := range []string{"beta", "Alpha", ".dotdir", "alpha2"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(home, "Alpha", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "plain.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Alpha", "nested"), filepath.Join(home, "link-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "plain.txt"), filepath.Join(home, "link-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "missing-target"), filepath.Join(home, "link-broken")); err != nil {
		t.Fatal(err)
	}

	code, listing := getDirs(t, m, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/dirs = %d", code)
	}
	if listing.Path != "~" {
		t.Fatalf("path = %q, want ~", listing.Path)
	}
	if listing.Parent != filepath.Dir(home) {
		t.Fatalf("parent = %q, want %q", listing.Parent, filepath.Dir(home))
	}
	wantDefault := []dirEntry{
		{Name: "Alpha", Path: "~/Alpha"},
		{Name: "alpha2", Path: "~/alpha2"},
		{Name: "beta", Path: "~/beta"},
		{Name: "link-dir", Path: "~/link-dir"},
	}
	if !reflect.DeepEqual(listing.Dirs, wantDefault) {
		t.Fatalf("dirs = %+v, want %+v", listing.Dirs, wantDefault)
	}

	code, hiddenListing := getDirs(t, m, "hidden=1")
	if code != http.StatusOK {
		t.Fatalf("GET /api/dirs?hidden=1 = %d", code)
	}
	wantHidden := []dirEntry{
		{Name: ".dotdir", Path: "~/.dotdir"},
		{Name: "Alpha", Path: "~/Alpha"},
		{Name: "alpha2", Path: "~/alpha2"},
		{Name: "beta", Path: "~/beta"},
		{Name: "link-dir", Path: "~/link-dir"},
	}
	if !reflect.DeepEqual(hiddenListing.Dirs, wantHidden) {
		t.Fatalf("hidden dirs = %+v, want %+v", hiddenListing.Dirs, wantHidden)
	}

	code, subListing := getDirs(t, m, "path="+url.QueryEscape("~/Alpha"))
	if code != http.StatusOK {
		t.Fatalf("GET ~/Alpha = %d", code)
	}
	if subListing.Path != "~/Alpha" || subListing.Parent != "~" {
		t.Fatalf("sub = %+v, want path=~/Alpha parent=~", subListing)
	}
	wantSub := []dirEntry{{Name: "nested", Path: "~/Alpha/nested"}}
	if !reflect.DeepEqual(subListing.Dirs, wantSub) {
		t.Fatalf("sub dirs = %+v, want %+v", subListing.Dirs, wantSub)
	}

	code, rootListing := getDirs(t, m, "path=/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	if rootListing.Path != "/" || rootListing.Parent != "" {
		t.Fatalf("root = %+v, want path=/ parent=\"\"", rootListing)
	}

	for _, bad := range []string{
		"relative/dir",
		filepath.Join(home, "does-not-exist"),
		filepath.Join(home, "plain.txt"),
	} {
		code, _ := getDirs(t, m, "path="+url.QueryEscape(bad))
		if code != http.StatusBadRequest {
			t.Fatalf("GET path=%q = %d, want 400", bad, code)
		}
	}
}
