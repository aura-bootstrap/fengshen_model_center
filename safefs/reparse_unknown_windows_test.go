//go:build windows

package safefs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

func TestUnknownGUIDReparseBoundary(t *testing.T) {
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "a-unknown")
			if directory {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("local file"), 0600); err != nil {
				t.Fatal(err)
			}
			const safe = "z-safe.bin"
			const contents = "safe sibling remains readable"
			if err := os.WriteFile(filepath.Join(root, safe), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := syscall.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
				syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
				nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.CloseHandle(h)

			const (
				tag         = uint32(0x00000042) // Non-Microsoft, non-name-surrogate tag; no target path.
				setPoint    = uint32(0x000900a4)
				getPoint    = uint32(0x000900a8)
				deletePoint = uint32(0x000900ac)
				headerSize  = 24
			)
			guid := [16]byte{0x8b, 0xc2, 0xb5, 0x61, 0x79, 0xb4, 0x7e, 0x43, 0xb2, 0xc1, 0x83, 0x09, 0x2d, 0x5a, 0x44, 0x17}
			payload := []byte{0x11, 0x22, 0x33, 0x44}
			buf := make([]byte, headerSize+len(payload))
			binary.LittleEndian.PutUint32(buf[0:4], tag)
			binary.LittleEndian.PutUint16(buf[4:6], uint16(len(payload)))
			copy(buf[8:24], guid[:])
			copy(buf[24:], payload)
			var returned uint32
			err = syscall.DeviceIoControl(h, setPoint, &buf[0], uint32(len(buf)), nil, 0, &returned, nil)
			if err != nil {
				var code syscall.Errno
				if !errors.As(err, &code) {
					t.Fatalf("FSCTL_SET_REPARSE_POINT: %v", err)
				}
				switch code {
				case 1, 5, 50, 1314:
					t.Skipf("real GUID reparse creation unavailable: FSCTL_SET_REPARSE_POINT Win32=%d (0x%x): %v; boundary NOT exercised", uint32(code), uint32(code), err)
				default:
					t.Fatalf("FSCTL_SET_REPARSE_POINT Win32=%d (0x%x): %v", uint32(code), uint32(code), err)
				}
			}
			// Delete only our tag through the original handle, before TempDir cleanup.
			defer func() {
				deletion := append([]byte(nil), buf[:headerSize]...)
				binary.LittleEndian.PutUint16(deletion[4:6], 0)
				if err := syscall.DeviceIoControl(h, deletePoint, &deletion[0], uint32(len(deletion)), nil, 0, &returned, nil); err != nil {
					t.Errorf("FSCTL_DELETE_REPARSE_POINT on owned handle: %v", err)
					return
				}
				st, err := os.Lstat(path)
				if err != nil {
					t.Error(err)
				} else if a, ok := st.Sys().(*syscall.Win32FileAttributeData); !ok || a.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
					t.Error("owned reparse attribute remains after deleting tag")
				}
			}()

			got := make([]byte, 16384)
			if err := syscall.DeviceIoControl(h, getPoint, nil, 0, &got[0], uint32(len(got)), &returned, nil); err != nil {
				t.Fatal("FSCTL_GET_REPARSE_POINT:", err)
			}
			if returned != uint32(len(buf)) || !bytes.Equal(got[:returned], buf) {
				t.Fatalf("real GUID reparse buffer mismatch: bytes=%d, got=%x, want=%x", returned, got[:returned], buf)
			}
			var info struct{ Attributes, Tag uint32 }
			query := syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")
			ok, _, queryErr := query.Call(uintptr(h), 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
			if ok == 0 {
				t.Fatal("FileAttributeTagInfo:", queryErr)
			}
			if info.Attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 || info.Tag != tag {
				t.Fatalf("FileAttributeTagInfo: attributes=0x%x tag=0x%x", info.Attributes, info.Tag)
			}
			s, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for label, lstat := range map[string]func() (os.FileInfo, error){
				"os.Lstat":   func() (os.FileInfo, error) { return os.Lstat(path) },
				"Root.Lstat": func() (os.FileInfo, error) { return s.root.Lstat("a-unknown") },
			} {
				st, err := lstat()
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				a, ok := st.Sys().(*syscall.Win32FileAttributeData)
				if !ok || a.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
					t.Fatalf("%s did not observe real reparse attributes: %+v", label, st.Sys())
				}
				if kind, err := reparse(s.root, "a-unknown", st); err != nil || kind != "reparse" {
					t.Fatalf("%s classification=%q: %v", label, kind, err)
				}
				t.Logf("%s attributes=0x%x; real FileAttributeTagInfo tag=0x%08x, GUID/payload verified", label, a.FileAttributes, info.Tag)
			}

			rels := []string{"a-unknown"}
			if directory {
				rels = append(rels, filepath.Join("a-unknown", "hidden.bin"), filepath.Join("a-unknown", "missing.bin"))
			}
			for _, rel := range rels {
				f, entry, err := s.OpenFile(rel)
				if f != nil {
					f.Close()
					t.Errorf("OpenFile(%q) returned a handle", rel)
				}
				if !errors.Is(err, ErrBoundary) || (rel == "a-unknown" && entry.LinkKind != "reparse") {
					t.Errorf("OpenFile(%q): entry=%+v err=%v", rel, entry, err)
				}
				derived, err := s.Derive(rel)
				if derived != nil {
					derived.Close()
					t.Errorf("Derive(%q) returned a scope", rel)
				}
				if !errors.Is(err, ErrBoundary) {
					t.Errorf("Derive(%q): %v", rel, err)
				}
			}
			seen := map[string]Entry{}
			err = s.Walk(context.Background(), Limits{MaxEntries: 16, MaxDepth: 4}, func(a Entry) error {
				seen[a.Relative] = a
				return nil
			})
			if !errors.Is(err, ErrPartial) || !errors.Is(err, ErrBoundary) {
				t.Fatalf("Walk must report partial boundary rejection: %v", err)
			}
			bad, good := seen["a-unknown"], seen[safe]
			if len(seen) != 2 || bad.LinkKind != "reparse" || bad.Error != ErrBoundary.Error() || bad.Identity != "" || good.Error != "" || good.LinkKind != "none" || good.Identity == "" || good.Size != int64(len(contents)) {
				t.Fatalf("Walk followed unknown reparse or lost safe sibling: %+v", seen)
			}
			derived, err := s.Derive(safe)
			if err != nil {
				t.Fatal("derive safe sibling:", err)
			}
			defer derived.Close()
			for _, scope := range []*Scope{s, derived} {
				f, a, err := scope.OpenFile(safe)
				if err != nil {
					t.Fatal("open safe sibling:", err)
				}
				data, readErr := io.ReadAll(f)
				closeErr := f.Close()
				if readErr != nil || closeErr != nil || string(data) != contents || a.Identity != good.Identity {
					t.Fatalf("safe sibling changed: data=%q entry=%+v read=%v close=%v", data, a, readErr, closeErr)
				}
			}
		})
	}
}
