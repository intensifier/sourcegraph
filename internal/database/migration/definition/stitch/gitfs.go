package stitch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sourcegraph/sourcegraph/internal/lazyregexp"
)

type gitFS struct {
	rev    string
	dir    string
	prefix string
}

// used to mock in tests
var newGitFS = defaultNewGitFS

// defaultNewGitFS returns a mock fileystem that uses `git show` to produce directory
// and file content. The given dir is used as the working directory of the git subcommand.
// The given prefix is the relative directory that roots the filesystem.
func defaultNewGitFS(rev, dir, prefix string) fs.FS {
	return &gitFS{rev, dir, prefix}
}

var gitTreePattern = regexp.MustCompile("^tree .+:.+\n")

func (fs *gitFS) Open(name string) (fs.File, error) {
	if name == "." || name == "/" {
		name = ""
	}
	out, err := fs.gitShow(name)
	if err != nil {
		return nil, err
	}

	if ok := gitTreePattern.Match(out); ok {
		lines := bytes.Split(out, []byte("\n"))

		entries := make([]string, 0, len(lines))
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}

			entries = append(entries, strings.TrimRight(string(line), string(os.PathSeparator)))
		}

		return &gitFSDir{
			name:    name,
			entries: entries,
		}, nil
	}

	return &gitFSFile{
		name:       name,
		ReadCloser: io.NopCloser(bytes.NewReader(out)),
	}, nil
}

func (fs *gitFS) gitShow(name string) (out []byte, err error) {
	filepath := filepath.Join(fs.prefix, name)

	defer func() {
		if replacedOut, ok := override(fs.rev, filepath, out); ok {
			out = replacedOut
			err = nil
		}
	}()

	cmd := exec.Command("git", "show", makeRevPath(fs.rev, filepath))
	cmd.Dir = fs.dir
	return cmd.CombinedOutput()
}

var alterExtensionPattern = lazyregexp.New(`(CREATE|COMMENT ON|DROP)\s+EXTENSION`)

var overrides = map[string]map[string]func(out []byte) ([]byte, bool){
	"v3.37.0": {
		// Add privileged directory
		"migrations/frontend": func(out []byte) ([]byte, bool) {
			return append(out, []byte("\n-1528395834")...), true
		},
		"migrations/frontend/-1528395834": func(out []byte) ([]byte, bool) {
			return append(out, []byte("tree v.37.0^:migrations/frontend/-1528395834\n\nmetadata.yaml\nup.sql\ndown.sql")...), true
		},
		"migrations/frontend/-1528395834/metadata.yaml": func(out []byte) ([]byte, bool) {
			return append(out, []byte("name: 'squashed migrations (privileged)'\nprivileged: true")...), true
		},
		"migrations/frontend/-1528395834/up.sql": func(out []byte) ([]byte, bool) {
			return append(out, []byte(`BEGIN;

CREATE EXTENSION IF NOT EXISTS citext;

COMMENT ON EXTENSION citext IS 'data type for case-insensitive character strings';

CREATE EXTENSION IF NOT EXISTS hstore;

COMMENT ON EXTENSION hstore IS 'data type for storing sets of (key, value) pairs';

CREATE EXTENSION IF NOT EXISTS intarray;

COMMENT ON EXTENSION intarray IS 'functions, operators, and index support for 1-D arrays of integers';

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

COMMENT ON EXTENSION pg_stat_statements IS 'track execution statistics of all SQL statements executed';

CREATE EXTENSION IF NOT EXISTS pg_trgm;

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';

END;
`)...), true
		},
		"migrations/frontend/-1528395834/down.sql": func(out []byte) ([]byte, bool) { return append(out, []byte("")...), true },

		// TODO - need to move privileged migrations
		"migrations/frontend/1528395834/up.sql": func(out []byte) ([]byte, bool) {
			return alterExtensionPattern.ReplaceAll(out, nil), true
		},
		"migrations/frontend/1528395834/metadata.yaml": func(out []byte) ([]byte, bool) {
			return append(out, []byte("\nparent: -1528395834")...), true
		},

		// This one had an extension addition
		"migrations/frontend/1528395953/metadata.yaml": func(out []byte) ([]byte, bool) {
			return append(out, []byte("\nprivileged: true\n")...), true
		},
	},
}

// TODO
func override(rev, path string, out []byte) ([]byte, bool) {
	if revOverrides, ok := overrides[rev]; ok {
		if pathOverrides, ok := revOverrides[path]; ok {
			return pathOverrides(out)
		}
	}

	return nil, false
}

type gitFSDir struct {
	name    string
	entries []string
	offset  int
}

func (d *gitFSDir) Stat() (fs.FileInfo, error) {
	return &gitDirEntry{name: d.name}, nil
}

func (d *gitFSDir) ReadDir(count int) ([]fs.DirEntry, error) {
	n := len(d.entries) - d.offset
	if n == 0 {
		if count <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	if count > 0 && n > count {
		n = count
	}

	list := make([]fs.DirEntry, 0, n)
	for i := 0; i < n; i++ {
		name := d.entries[d.offset]
		list = append(list, &gitDirEntry{name: name})
		d.offset++
	}

	return list, nil
}

func (d *gitFSDir) Read(_ []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *gitFSDir) Close() error {
	return nil
}

type gitDirEntry struct {
	name string
}

func (e *gitDirEntry) Name() string               { return e.name }
func (e *gitDirEntry) Size() int64                { return 0 }
func (e *gitDirEntry) Mode() fs.FileMode          { return fs.ModeDir }
func (e *gitDirEntry) ModTime() time.Time         { return time.Time{} }
func (e *gitDirEntry) IsDir() bool                { return e.Mode().IsDir() }
func (e *gitDirEntry) Sys() any                   { return nil }
func (e *gitDirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (e *gitDirEntry) Info() (fs.FileInfo, error) { return e, nil }

type gitFSFile struct {
	name string
	size int64
	io.ReadCloser
}

func (f *gitFSFile) Stat() (fs.FileInfo, error) {
	return &gitFileEntry{name: f.name, size: f.size}, nil
}

func (d *gitFSFile) ReadDir(count int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("not a directory")}
}

type gitFileEntry struct {
	name string
	size int64
}

func (e *gitFileEntry) Name() string       { return e.name }
func (e *gitFileEntry) Size() int64        { return e.size }
func (e *gitFileEntry) Mode() fs.FileMode  { return fs.ModePerm }
func (e *gitFileEntry) ModTime() time.Time { return time.Time{} }
func (e *gitFileEntry) IsDir() bool        { return e.Mode().IsDir() }
func (e *gitFileEntry) Sys() any           { return nil }

func makeRevPath(rev, path string) string {
	return fmt.Sprintf("%s^:%s", rev, path)
}
