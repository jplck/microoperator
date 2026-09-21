//go:build darwin || linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedSourceAndProfileFailClosed(t *testing.T) {
	for _, source := range []string{`package main; import "example.com/tool"`, `package main; import "C"`, `package other`, `package main; import "../secret"`, `broken`} {
		if err := validateGeneratedSource(source); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
	if err := validateGeneratedSource(`package main; import "strings"; func Process(s string)(string,error){return strings.ToUpper(s),nil}`); err != nil {
		t.Fatal(err)
	}
	if err := generatedProfile(sandboxConfig{}); err == nil {
		t.Fatal("generated work accepted an unqualified profile")
	}
}

func TestToolchainDigestIncludesContentAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "compiler")
	if err := os.WriteFile(file, []byte("one"), 0700); err != nil {
		t.Fatal(err)
	}
	first, err := toolchainDigest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0700); err != nil {
		t.Fatal(err)
	}
	second, err := toolchainDigest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("toolchain mutation not detected")
	}
	if err := os.Symlink(file, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := toolchainDigest(context.Background(), root); err == nil {
		t.Fatal("toolchain symlink accepted")
	}
}
