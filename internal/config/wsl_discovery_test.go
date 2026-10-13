package config

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf16"
)

func TestWSLDistroNamesPreserveUnicode(t *testing.T) {
	want := []string{"Ubuntu-开发", "仕事-🚀"}
	input := "Ubuntu-开发\r\n仕事-🚀\r\n"
	for _, bom := range []bool{false, true} {
		var raw []byte
		if bom {
			raw = append(raw, 0xff, 0xfe)
		}
		for _, unit := range utf16.Encode([]rune(input)) {
			raw = binary.LittleEndian.AppendUint16(raw, unit)
		}
		if got := parseWSLList(raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("BOM=%v: got %q, want %q", bom, got, want)
		}
	}
	if got := parseWSLList([]byte(input)); !reflect.DeepEqual(got, want) {
		t.Fatalf("UTF-8: %q", got)
	}
}

func TestWSLHomesIncludesRootWithoutHomeDirectory(t *testing.T) {
	root := t.TempDir()
	rootHome := filepath.Join(root, "root")
	if err := os.Mkdir(rootHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := wslHomesUnder(root); !reflect.DeepEqual(got, []string{rootHome}) {
		t.Fatalf("root-only distro: %v", got)
	}
	userHome := filepath.Join(root, "home", "alice")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := wslHomesUnder(root); !reflect.DeepEqual(got, []string{userHome, rootHome}) {
		t.Fatalf("multi-user distro: %v", got)
	}
}
