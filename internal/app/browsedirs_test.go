package app

import (
	"io/fs"
	"reflect"
	"testing"
	"time"
)

type fakeDirEntry struct {
	name string
	dir  bool
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.dir }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return fakeFileInfo{name: e.name, dir: e.dir}, nil }

type fakeFileInfo struct {
	name string
	dir  bool
}

func (i fakeFileInfo) Name() string { return i.name }
func (i fakeFileInfo) Size() int64  { return 0 }
func (i fakeFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i fakeFileInfo) IsDir() bool        { return i.dir }
func (i fakeFileInfo) Sys() any           { return nil }

func TestBrowseDirsWindowsPathQueries(t *testing.T) {
	entries := []fs.DirEntry{
		fakeDirEntry{name: "alpha", dir: true},
		fakeDirEntry{name: ".hidden", dir: true},
		fakeDirEntry{name: "note.txt", dir: false},
	}
	tests := []struct {
		name       string
		query      string
		wantPrefix string
		want       []string
	}{
		{
			name:       "drive backslash",
			query:      `C:\Users\x`,
			wantPrefix: `C:\Users\`,
			want:       []string{"base", `C:\Users\alpha`},
		},
		{
			name:       "drive slash",
			query:      `C:/Users/x`,
			wantPrefix: `C:/Users/`,
			want:       []string{"base", `C:/Users/alpha`},
		},
		{
			name:       "backslash mid path",
			query:      `projects\ax`,
			wantPrefix: `projects\`,
			want:       []string{"base", `projects\alpha`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPrefix string
			got := browseDirsWithReadDir(tt.query, []string{"base"}, func(prefix string) ([]fs.DirEntry, error) {
				gotPrefix = prefix
				return entries, nil
			})
			if gotPrefix != tt.wantPrefix {
				t.Fatalf("read prefix = %q, want %q", gotPrefix, tt.wantPrefix)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("browseDirs = %#v, want %#v", got, tt.want)
			}
		})
	}
}
