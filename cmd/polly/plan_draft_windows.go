package main

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openRegularDraft(root *os.Root, name string) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	path := filepath.Join(root.Name(), name)
	h, err := openDraftHandle(dir, name, windows.FILE_GENERIC_READ)
	if err != nil {
		return nil, &os.PathError{Op: "open draft", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func singleLinkDraft(f *os.File, _ os.FileInfo) bool {
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) == nil && info.NumberOfLinks == 1
}

// Open exactly one name relative to the pinned directory without resolving
// reparse points. All caller-supplied names are managed basenames.
func openDraftHandle(dir *os.File, name string, access uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(dir.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return 0, draftWinError(err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("refusing to follow a reparse point")
	}
	return h, nil
}

func renameDraftNoReplace(dir *os.File, from, to string) error {
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	// FILE_RENAME_INFORMATION, with ReplaceIfExists left false. The kernel
	// ignores the bytes beyond FileNameLength. Names are at most one component.
	info := struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [256]uint16
	}{RootDirectory: windows.Handle(dir.Fd()), FileNameLength: uint32((len(name) - 1) * 2)}
	if len(name) > len(info.FileName) {
		return windows.ERROR_INVALID_NAME
	}
	copy(info.FileName[:], name)
	h, err := openDraftHandle(dir, from, windows.DELETE|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return draftWinError(windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), windows.FileRenameInformation))
}

func draftWinError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}
