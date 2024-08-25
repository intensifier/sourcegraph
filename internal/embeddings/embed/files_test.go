package embed

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sourcegraph/sourcegraph/internal/paths"
)

func TestExcludingFilePaths(t *testing.T) {
	files := []string{
		"file.sql",
		"root/file.yaml",
		"client/web/struct.json",
		"vendor/vendor.txt",
		"cool.go",
		"node_modules/a.go",
		"Dockerfile",
		"README.md",
		"vendor/README.md",
		"LICENSE.txt",
		"nested/vendor/file.py",
		".prettierignore",
		"client/web/.gitattributes",
		"no_ignore",
		"data/names.csv",
	}

	expectedFiles := []string{
		"file.sql",
		"root/file.yaml",
		"client/web/struct.json",
		"cool.go",
		"Dockerfile",
		"README.md",
		"LICENSE.txt",
		"no_ignore",
	}

	var gotFiles []string

	excludedGlobPatterns := GetDefaultExcludedFilePathPatterns()
	for _, file := range files {
		if !isExcludedFilePathMatch(file, excludedGlobPatterns) {
			gotFiles = append(gotFiles, file)
		}
	}

	require.Equal(t, expectedFiles, gotFiles)
}

func TestIncludingFilePaths(t *testing.T) {
	files := []string{
		"file.sql",
		"root/file.yaml",
		"client/web/struct.json",
		"vendor/vendor.txt",
		"cool.go",
		"cmd/a.go",
		"Dockerfile",
		"README.md",
		"vendor/README.md",
		"LICENSE.txt",
		"nested/vendor/file.py",
		".prettierignore",
		"client/web/.gitattributes",
		"no_ignore",
		"data/names.csv",
	}

	expectedFiles := []string{
		"cool.go",
		"cmd/a.go",
	}

	var gotFiles []string
	pattern := "*.go"
	g, err := paths.Compile(pattern)
	require.Nil(t, err)
	includedGlobPatterns := []*paths.GlobPattern{g}
	for _, file := range files {
		if isIncludedFilePathMatch(file, includedGlobPatterns) {
			gotFiles = append(gotFiles, file)
		}
	}

	require.Equal(t, expectedFiles, gotFiles)
}

func TestIncludingFilePathsWithEmptyIncludes(t *testing.T) {
	files := []string{
		"file.sql",
		"root/file.yaml",
		"client/web/struct.json",
		"vendor/vendor.txt",
		"cool.go",
		"cmd/a.go",
		"Dockerfile",
		"README.md",
		"vendor/README.md",
		"LICENSE.txt",
		"nested/vendor/file.py",
		".prettierignore",
		"client/web/.gitattributes",
		"no_ignore",
		"data/names.csv",
	}

	var gotFiles []string
	for _, file := range files {
		if isIncludedFilePathMatch(file, []*paths.GlobPattern{}) {
			gotFiles = append(gotFiles, file)
		}
	}

	require.Equal(t, files, gotFiles)
}

func Test_isEmbeddableFileContent(t *testing.T) {
	cases := []struct {
		content    []byte
		embeddable bool
		reason     SkipReason
	}{{
		// gob file header
		content:    bytes.Repeat([]byte{0xff, 0x0f, 0x04, 0x83, 0x02, 0x01, 0x84, 0xff, 0x01, 0x00}, 10),
		embeddable: false,
		reason:     SkipReasonBinary,
	}, {
		content:    []byte("test"),
		embeddable: false,
		reason:     SkipReasonSmall,
	}, {
		content:    []byte("file that is larger than the minimum size but contains the word lockfile"),
		embeddable: false,
		reason:     SkipReasonAutogenerated,
	}, {
		content:    []byte("file that is larger than the minimum\nsize but contains the words do not edit"),
		embeddable: false,
		reason:     SkipReasonAutogenerated,
	}, {
		content:    bytes.Repeat([]byte("very long line "), 1000),
		embeddable: false,
		reason:     SkipReasonLongLine,
	}, {
		content:    bytes.Repeat([]byte("somewhat long line "), 10),
		embeddable: true,
		reason:     SkipReasonNone,
	}}

	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			emeddable, skipReason := isEmbeddableFileContent(tc.content)
			require.Equal(t, tc.embeddable, emeddable)
			require.Equal(t, tc.reason, skipReason)
		})
	}
}

func TestIsValidTextFile(t *testing.T) {
	tests := []struct {
		fileName string
		want     bool
	}{
		{
			fileName: "docs.asciidoc",
			want:     true,
		},
		{
			fileName: "CHANGELOG.md",
			want:     true,
		},
		{
			fileName: "LICENSE.txt",
			want:     true,
		},
		{
			fileName: "License.MD",
			want:     true,
		},
		{
			fileName: "data.bin",
			want:     false,
		},
		{
			fileName: "rst.java",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.fileName, func(t *testing.T) {
			got := IsValidTextFile(tt.fileName)
			if got != tt.want {
				t.Errorf("IsValidTextFile() got = %v, want %v", got, tt.want)
			}
		})
	}
}
