package fs

import (
	"os"
	"strings"
	"syscall"
)

func (fs *InMemoryFileSystem) mkdirCommon(name string, perm os.FileMode, all bool) error {
	if (perm & os.ModeType) != 0 {
		return errBadMode
	}
	n, nameRem, err := fs.find(name, false, true)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !all {
		slashPos := strings.IndexByte(nameRem, '/')
		if slashPos >= 0 {
			return os.ErrNotExist
		}
		if nameRem == "" {
			return os.ErrExist
		}
	} else {
		if !n.mode.IsDir() {
			return syscall.ENOTDIR
		}
		fs.mkdirCommonAll(n, nameRem, perm)
		return nil
	}
	n.dirAppend(newDirNode(
		perm,
		nameRem,
	))
	return nil
}

func (fs *InMemoryFileSystem) mkdirCommonAll(n *node, nameRem string, perm os.FileMode) {
	for nameRem != "" {
		slashPos := strings.IndexByte(nameRem, '/')
		nameComp := nameRem
		if slashPos >= 0 {
			nameComp = nameComp[:slashPos]
		}
		if nameComp != "" {
			validateNameComp(nameComp)
			childN := newDirNode(
				perm,
				nameComp,
			)
			n.dirAppend(childN)
			n = childN
		}
		if slashPos < 0 {
			nameRem = ""
		} else {
			nameRem = nameRem[slashPos+1:]
		}
	}
}

func (fs *InMemoryFileSystem) Mkdir(name string, perm os.FileMode) error {
	return fs.mkdirCommon(name, perm, false)
}

func (fs *InMemoryFileSystem) MkdirAll(name string, perm os.FileMode) error {
	return fs.mkdirCommon(name, perm, true)
}
