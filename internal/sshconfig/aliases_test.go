package sshconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAliasesReadsHostAndIncludes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(filepath.Join(sshDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(mainPath, []byte(`
Host main *.corp !blocked
  HostName example.com
Include conf.d/*.conf
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "conf.d", "extra.conf"), []byte(`
Host extra second
Include loop.conf
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "conf.d", "loop.conf"), []byte(`
Include ../config
Host looped
`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Aliases(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"extra", "looped", "main", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %#v, want %#v", got, want)
	}
}

func TestHasAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("Host one two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := HasAlias(path, "two")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("HasAlias(two) = false")
	}
	ok, err = HasAlias(path, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("HasAlias(missing) = true")
	}
}
