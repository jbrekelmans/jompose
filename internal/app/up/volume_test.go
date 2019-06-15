package up

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/kube-compose/kube-compose/internal/pkg/fs"
)

var errTest = fmt.Errorf("test error")
var testFileContent = "content"

var vfs *fs.InMemoryFileSystem

// init here is justified because a common mock file system is used, and we require calling Set to make tests deterministic.
// nolint
func init() {
	vfs = fs.NewInMemoryFileSystem(map[string]fs.InMemoryFile{
		"/orig": {
			Content: []byte(testFileContent),
		},
		"/origerr": {
			Error: errTest,
		},
	})
	vfs.Set("/dir/file1", fs.InMemoryFile{
		Content: []byte(testFileContent),
	})
	vfs.Set("/dir/file2", fs.InMemoryFile{
		Content: []byte(testFileContent),
	})
	vfs.Set("/dir2/file", fs.InMemoryFile{
		Content: []byte(testFileContent),
	})
	vfs.Set("/dir2/symlink", fs.InMemoryFile{
		Content: []byte("file"),
		Mode:    os.ModeSymlink,
	})
	vfs.Set("/dir3/symlink", fs.InMemoryFile{
		Content: []byte("/dir2"),
		Mode:    os.ModeSymlink,
	})
}

func withMockFS(vfs fs.VirtualFileSystem, cb func()) {
	orig := fs.OS
	defer func() {
		fs.OS = orig
	}()
	fs.OS = vfs
	cb()
}

type mockTarWriterEntry struct {
	h    *tar.Header
	data []byte
}

type mockTarWriter struct {
	entries []mockTarWriterEntry
}

func (m *mockTarWriter) WriteHeader(header *tar.Header) error {
	m.entries = append(m.entries, mockTarWriterEntry{
		h: header,
	})
	return nil
}

func (m *mockTarWriter) Write(p []byte) (int, error) {
	entry := &m.entries[len(m.entries)-1]
	entry.data = append(entry.data, p...)
	return len(p), nil
}

func regularFile(name, data string) mockTarWriterEntry {
	return mockTarWriterEntry{
		h: &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Size:     int64(len(data)),
		},
		data: []byte(data),
	}
}

func symlink(name, link string) mockTarWriterEntry {
	return mockTarWriterEntry{
		h: &tar.Header{
			Name:     name,
			Typeflag: tar.TypeSymlink,
			Linkname: link,
		},
	}
}

func directory(name string) mockTarWriterEntry {
	return mockTarWriterEntry{
		h: &tar.Header{
			Name:     name,
			Typeflag: tar.TypeDir,
		},
	}
}

func Test_BindMountHostFileToTar_SuccessRegularFile(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		isDir, err := bindMountHostFileToTar(tw, "orig", "renamed")
		if err != nil {
			t.Error(err)
		} else {
			if isDir {
				t.Fail()
			}
			expected := []mockTarWriterEntry{
				regularFile("renamed", testFileContent),
			}
			if !reflect.DeepEqual(tw.entries, expected) {
				t.Logf("entries1: %+v\n", tw.entries)
				t.Logf("entries2: %+v\n", expected)
				t.Fail()
			}
		}
	})
}

func Test_BindMountHostFileToTar_RecoverFromRegularFileError(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		isDir, err := bindMountHostFileToTar(tw, "origerr", "renamed2")
		if err != nil {
			t.Error(err)
		} else {
			if !isDir {
				t.Fail()
			}
			expected := []mockTarWriterEntry{
				directory("renamed2/"),
			}
			if !reflect.DeepEqual(tw.entries, expected) {
				t.Logf("entries1: %+v\n", tw.entries)
				t.Logf("entries2: %+v\n", expected)
				t.Fail()
			}
		}
	})
}
func Test_BindMountHostFileToTar_RegularFileTarError(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		isDir, err := bindMountHostFileToTar(tw, "origerr", "renamed")
		if err != nil {
			t.Error(err)
		} else {
			if !isDir {
				t.Fail()
			}
			expected := []mockTarWriterEntry{
				directory("renamed/"),
			}
			if !reflect.DeepEqual(tw.entries, expected) {
				t.Logf("entries1: %+v\n", tw.entries)
				t.Logf("entries2: %+v\n", expected)
				t.Fail()
			}
		}
	})
}

func Test_BindMountHostFileToTar_SuccessDir(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		isDir, err := bindMountHostFileToTar(tw, "dir", "renamed")
		if err != nil {
			t.Error(err)
		} else {
			if !isDir {
				t.Fail()
			}
			expected := []mockTarWriterEntry{
				directory("renamed/"),
				regularFile("renamed/file1", testFileContent),
				regularFile("renamed/file2", testFileContent),
			}
			if !reflect.DeepEqual(tw.entries, expected) {
				t.Logf("entries1: %+v\n", tw.entries)
				t.Logf("entries2: %+v\n", expected)
				t.Fail()
			}
		}
	})
}

func Test_BindMountHostFileToTar_SuccessSymlink(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		isDir, err := bindMountHostFileToTar(tw, "dir2", "renamed")
		if err != nil {
			t.Error(err)
		} else {
			if !isDir {
				t.Fail()
			}
			expected := []mockTarWriterEntry{
				directory("renamed/"),
				regularFile("renamed/file", testFileContent),
				symlink("renamed/symlink", "file"),
			}
			if !reflect.DeepEqual(tw.entries, expected) {
				t.Logf("entries1: %+v\n", tw.entries)
				t.Logf("entries2: %+v\n", expected)
				t.Fail()
			}
		}
	})
}

func Test_BindMountHostFileToTar_ErrorSymlinkNotWithinBindHostRoot(t *testing.T) {
	withMockFS(vfs, func() {
		tw := &mockTarWriter{}
		_, err := bindMountHostFileToTar(tw, "dir3", "renamed")
		if err == nil {
			t.Fail()
		}
	})
}

func Test_BuildVolumeInitImageGetDockerfile_Success(t *testing.T) {
	actual := buildVolumeInitImageGetDockerfile([]bool{true, false})
	expected := []byte(`ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY data1/ /app/data/vol1/
COPY data2 /app/data/vol2
ENTRYPOINT ["bash", "-c", "cp -ar /app/data/vol1 /mnt/vol1/root && cp -ar /app/data/vol2 /mnt/vol2/root"]
`)
	if !bytes.Equal(actual, expected) {
		t.Logf("actual:\n%s", string(actual))
		t.Logf("expected:\n%s", string(expected))
		t.Fail()
	}
}

func Test_ResolveBindVolumeHostPath_AbsError(t *testing.T) {
	errExpected := fmt.Errorf("resolveBindVolumeHostPathAbsError")
	vfs := fs.NewInMemoryFileSystem(map[string]fs.InMemoryFile{})
	vfs.AbsError = errExpected
	fsOld := fs.OS
	defer func() {
		fs.OS = fsOld
	}()
	fs.OS = vfs

	_, errActual := resolveBindVolumeHostPath("")
	if errActual != errExpected {
		t.Fail()
	}
}

func Test_ResolveBindVolumeHostPath_SuccessMkdirAll(t *testing.T) {
	vfs := fs.NewInMemoryFileSystem(map[string]fs.InMemoryFile{})
	fsOld := fs.OS
	defer func() {
		fs.OS = fsOld
	}()
	fs.OS = vfs

	resolved, err := resolveBindVolumeHostPath("/dir1/dir1_1")
	switch {
	case err != nil:
		t.Error(err)
	case resolved != "/dir1/dir1_1":
		t.Fail()
	default:
		fileInfo, err := fs.OS.Stat("/dir1/dir1_1")
		if err != nil {
			t.Error(err)
		} else if !fileInfo.IsDir() {
			t.Fail()
		}
	}
}
