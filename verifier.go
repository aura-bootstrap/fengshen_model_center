package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
	"io"
	"strconv"
	"strings"
	"time"
)

const fingerprintNotice = "仅表示本次读取字节的 SHA-256；不证明原子快照、发行方认证、无恶意内容、格式正确或推理可用。"

func (m *Manager) StartVerify(ctx context.Context, inventoryID, modelID string, fileIDs []string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv := m.inventories[inventoryID]
	if inv == nil {
		return Operation{}, m.inventoryMissing(inventoryID)
	}
	if inv.Stale || inv.RegistryRevision != m.registry.Snapshot().Revision || inventoryID != m.current {
		return Operation{}, failure("inventory_stale", "盘点已过期，请重新扫描", 409)
	}
	if len(fileIDs) == 0 || len(fileIDs) > 1000 {
		return Operation{}, failure("request_invalid", "请选择 1–1000 个文件 ID", 422)
	}
	allowedFiles := map[string]*ModelFile{}
	for _, s := range inv.Models {
		if s.ID == modelID {
			for _, f := range s.files {
				allowedFiles[f.ID] = f
			}
		}
	}
	files := make([]*ModelFile, 0, len(fileIDs))
	seen := map[string]bool{}
	for _, id := range fileIDs {
		f := allowedFiles[id]
		if f == nil || seen[id] {
			return Operation{}, failure("file_not_allowed", "文件 ID 不属于当前模型或重复", 422)
		}
		seen[id] = true
		files = append(files, f)
	}
	o, job, e := m.begin(ctx, "verify", m.verifyTimeout)
	if e != nil {
		return Operation{}, e
	}
	for _, f := range files {
		f.Hash = nil
	}
	refreshContentEqual(inv)
	go func() {
		defer m.wg.Done()
		buf := make([]byte, 1024*1024)
		hashes := make([]HashRecord, 0, len(files))
		var processed int64
		var anyFailed bool
		for _, f := range files {
			h := m.fingerprint(job, inv.access[f.ScopeID], f, buf)
			hashes = append(hashes, h)
			if h.Status != "fingerprint_computed" {
				anyFailed = true
			}
			n, _ := strconv.ParseInt(h.Bytes, 10, 64)
			processed += n
			m.mu.Lock()
			if op := m.operations[o.ID]; op != nil {
				op.CheckedEntries = len(hashes)
				op.ProcessedBytes = strconv.FormatInt(processed, 10)
			}
			if h.Status == "fingerprint_computed" {
				v := h
				f.Hash = &v
			} else {
				f.Hash = nil
			}
			m.mu.Unlock()
		}
		var err error
		if anyFailed {
			err = errors.New("部分文件校验失败或已失效；失败文件不产生有效摘要")
		}
		m.finish(o.ID, job, nil, hashes, nil, nil, err)
	}()
	return cloneJSON(*o), nil
}
func (m *Manager) fingerprint(ctx context.Context, scope *accessScope, file *ModelFile, buf []byte) HashRecord {
	h := HashRecord{FileID: file.ID, Status: "failed", CheckedAt: time.Now().UTC(), Bytes: "0", Message: fingerprintNotice}
	fail := func(msg string) HashRecord {
		h.Message = msg + "；" + fingerprintNotice
		h.SHA256 = ""
		h.CheckedAt = time.Now().UTC()
		if ctx.Err() != nil {
			h.Status = "cancelled"
		}
		return h
	}
	if e := ctx.Err(); e != nil {
		return fail(e.Error())
	}
	if scope == nil {
		return fail("授权范围已失效")
	}
	f, before, e := scope.scope.OpenFile(file.relative)
	if e != nil {
		return fail("file_unreadable: " + e.Error())
	}
	defer f.Close()
	if !sameFile(file, before) {
		return fail("file_changed: 文件已变化，请重新扫描")
	}
	hash := sha256.New()
	var size int64
	for {
		if m.beforeRead != nil {
			m.beforeRead(ctx)
		}
		if e = ctx.Err(); e != nil {
			return fail(e.Error())
		}
		var n int
		n, e = f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			size += int64(n)
			h.Bytes = strconv.FormatInt(size, 10)
		}
		if e != nil {
			if !errors.Is(e, io.EOF) {
				return fail("file_unreadable: " + e.Error())
			}
			break
		}
		if n == 0 {
			return fail("file_unreadable: 无读取进展")
		}
	}
	if m.afterRead != nil {
		m.afterRead(ctx)
	}
	after, e := f.Stat()
	if e != nil {
		return fail("file_unreadable: " + e.Error())
	}
	identity, e := safefs.Identity(f)
	if e != nil {
		return fail("file_changed: 无法复核句柄身份")
	}
	if size != before.Size || after.Size() != before.Size || !after.ModTime().Equal(before.Modified) || identity != before.Identity {
		return fail("file_changed: 读取期间文件发生变化")
	}
	check, pathEntry, e := scope.scope.OpenFile(file.relative)
	if check != nil {
		check.Close()
	}
	if e != nil || !sameFile(file, pathEntry) {
		return fail("file_changed: 读取后路径身份或元数据已变化")
	}
	if e = ctx.Err(); e != nil {
		return fail(e.Error())
	}
	h.Status = "fingerprint_computed"
	h.SHA256 = hex.EncodeToString(hash.Sum(nil))
	h.CheckedAt = time.Now().UTC()
	return h
}

// refreshContentEqual uses linear-sized groups instead of all pairs. Only current
// fingerprints of distinct ordinary file objects participate; accounting is untouched.
// Callers hold the manager lock after the inventory has been published.
func refreshContentEqual(inv *Inventory) {
	groups := map[string][]*ModelFile{}
	for _, s := range inv.Models {
		for _, f := range s.files {
			f.ContentEqual = ""
			h := f.Hash
			if inv.Stale || h == nil || h.Status != "fingerprint_computed" || h.FileID != f.ID || h.Bytes != f.Bytes || f.LinkKind != "none" {
				continue
			}
			digest, err := hex.DecodeString(h.SHA256)
			if err != nil || len(digest) != sha256.Size {
				continue
			}
			key := strings.ToLower(h.SHA256) + ":" + f.Bytes
			groups[key] = append(groups[key], f)
		}
	}
	for key, files := range groups {
		objects := map[string]bool{}
		for _, f := range files {
			objects[fileKey(safefs.Entry{Path: f.Path, Identity: f.Identity})] = true
		}
		if len(objects) < 2 {
			continue
		}
		group := sha256.Sum256([]byte(inv.ID + "\x00" + key))
		for _, f := range files {
			f.ContentEqual = hex.EncodeToString(group[:])
		}
	}
}

func sameFile(f *ModelFile, e safefs.Entry) bool {
	return f.Identity == e.Identity && f.size == e.Size && f.Modified.Equal(e.Modified)
}
