package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

const trustLimit = 64 * 1024
const trustPath = "model-center/engine-trust.json"

type engineTrustInput struct {
	InputPath  string `json:"inputpath"`
	Edition    string `json:"edition"`
	Management *bool  `json:"management"`
}
type engineTrustTemplate struct {
	Schema  int                `json:"schema"`
	Engines []engineTrustInput `json:"engines"`
}
type engineTrustOutput struct {
	Schema         int                         `json:"schema"`
	CenterSHA256   string                      `json:"center_sha256"`
	DeliverySHA256 string                      `json:"delivery_sha256"`
	Engines        []integration.TrustedEngine `json:"engines"`
}

func trustDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}

// os.FileInfo exposes Windows reparse attributes even for unknown tags whose
// Mode does not identify a symlink. Reflection keeps this offline tool portable.
func trustOrdinaryRoot(root string) error {
	if e := safefs.ValidatePath(root); e != nil {
		return e
	}
	for p := filepath.Clean(root); ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return safefs.ErrBoundary
		}
		v := reflect.ValueOf(st.Sys())
		if v.IsValid() && v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
			if v.Kind() == reflect.Struct {
				a := v.FieldByName("FileAttributes")
				if a.IsValid() && a.Kind() == reflect.Uint32 && a.Uint()&0x400 != 0 {
					return safefs.ErrBoundary
				}
			}
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func trustTemplateJSON(b []byte, v *engineTrustTemplate) error {
	if len(b) > trustLimit || !utf8.Valid(b) {
		return fmt.Errorf("invalid engine template size/UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	for {
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if tok == nil {
			return fmt.Errorf("null engine template field")
		}
	}
	var doc map[string]json.RawMessage
	if e := json.Unmarshal(b, &doc); e != nil {
		return e
	}
	for k := range doc {
		if k != "schema" && k != "engines" {
			return fmt.Errorf("unknown engine template field")
		}
	}
	var entries []map[string]json.RawMessage
	if e := json.Unmarshal(doc["engines"], &entries); e != nil {
		return e
	}
	for _, entry := range entries {
		for k := range entry {
			if k != "inputpath" && k != "edition" && k != "management" {
				return fmt.Errorf("unknown engine entry field")
			}
		}
	}
	return components.StrictJSON(b, v)
}

// engineTrust runs only after final engine compilation. No delivery rewrite or
// engine ldflags depend on this output, so there is no digest feedback cycle.
func engineTrust(ctx context.Context, root, template, centerSHA, deliverySHA string) error {
	if !trustDigest(centerSHA) || !trustDigest(deliverySHA) {
		return fmt.Errorf("valid -center-sha256 and -delivery-sha256 are required")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	f, e := os.Open(template)
	if e != nil {
		return e
	}
	b, e := io.ReadAll(io.LimitReader(f, trustLimit+1))
	f.Close()
	if e != nil {
		return e
	}
	var input engineTrustTemplate
	if e = trustTemplateJSON(b, &input); e != nil {
		return e
	}
	if input.Schema != 1 || len(input.Engines) < 1 || len(input.Engines) > 32 {
		return fmt.Errorf("engine template requires schema 1 and 1..32 engines")
	}
	if e = trustOrdinaryRoot(root); e != nil {
		return e
	}
	s, e := safefs.Open(root)
	if e != nil {
		return e
	}
	defer s.Close()
	type observed struct {
		rel   string
		entry safefs.Entry
	}
	var observations []observed
	remember := func(rel string) error {
		leaf, e := s.Derive(rel)
		if e != nil {
			return e
		}
		f, a, e := leaf.OpenFile(filepath.Base(rel))
		if f != nil {
			f.Close()
		}
		leaf.Close()
		if e == nil {
			observations = append(observations, observed{rel, a})
		}
		return e
	}
	for _, rel := range []string{centerPath, "model-center/delivery.json"} {
		if e = remember(rel); e != nil {
			return e
		}
	}
	center, e := hash(ctx, s, centerPath)
	if e != nil {
		return e
	}
	deliveryFile, e := hash(ctx, s, "model-center/delivery.json")
	if e != nil {
		return e
	}
	if center.SHA256 != centerSHA || deliveryFile.SHA256 != deliverySHA {
		return fmt.Errorf("center/delivery digest mismatch")
	}
	raw, e := deliveryBytes(s, deliveryFile)
	if e != nil {
		return e
	}
	var d Delivery
	if e = components.StrictJSON(raw, &d); e != nil {
		return e
	}
	if d.Schema != 1 || d.Inference != "not_tested" || d.CleanEnvironment != "not_verified" {
		return fmt.Errorf("invalid delivery schema/evidence")
	}
	found := false
	for _, item := range d.Files {
		if item.Path == centerPath {
			if item.SHA256 != centerSHA || item.Bytes != center.Bytes {
				return fmt.Errorf("delivery center binding mismatch")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("delivery center missing")
	}
	if e = validateDelivery(ctx, s, d); e != nil {
		return e
	}
	out := engineTrustOutput{Schema: 1, CenterSHA256: centerSHA, DeliverySHA256: deliverySHA}
	seen := map[string]bool{}
	for _, v := range input.Engines {
		key := strings.ToLower(v.InputPath)
		if !components.SafeRelative(v.InputPath) || !strings.EqualFold(filepath.Ext(v.InputPath), ".exe") || seen[key] || v.Management == nil || (v.Edition != "user" && v.Edition != "admin" && v.Edition != "developer") || v.Edition == "user" && *v.Management {
			return fmt.Errorf("invalid/duplicate engine input")
		}
		seen[key] = true
		if e = remember(v.InputPath); e != nil {
			return e
		}
		item, e := hash(ctx, s, v.InputPath)
		if e != nil {
			return e
		}
		if item.Bytes == "0" {
			return fmt.Errorf("empty engine")
		}
		out.Engines = append(out.Engines, integration.TrustedEngine{Path: v.InputPath, SHA256: item.SHA256, Edition: v.Edition, Management: *v.Management})
	}
	for _, o := range observations {
		leaf, e := s.Derive(o.rel)
		if e != nil {
			return e
		}
		f, a, e := leaf.OpenFile(filepath.Base(o.rel))
		if f != nil {
			f.Close()
		}
		leaf.Close()
		if e != nil || a.Identity != o.entry.Identity || a.Size != o.entry.Size || !a.Modified.Equal(o.entry.Modified) {
			return fmt.Errorf("build input changed")
		}
	}
	b, e = json.MarshalIndent(out, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > trustLimit {
		return fmt.Errorf("engine trust exceeds 64 KiB")
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = trustOrdinaryRoot(root); e != nil {
		return e
	}
	dir, e := s.Derive("model-center")
	if e != nil {
		return e
	}
	defer dir.Close()
	target, e := os.OpenRoot(dir.Path())
	if e != nil {
		return e
	}
	defer target.Close()
	// Anchor the writer and confirm it is still the checked directory.
	anchor, e := target.Open(".")
	if e != nil {
		return e
	}
	id, e := safefs.Identity(anchor)
	anchor.Close()
	if e != nil {
		return e
	}
	check, entry, e := s.OpenDirectory("model-center")
	if check != nil {
		check.Close()
	}
	if e != nil || entry.Identity != id {
		return safefs.ErrBoundary
	}
	file, e := target.OpenFile("engine-trust.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(b)
	if e == nil {
		e = file.Sync()
	}
	ce := file.Close()
	if e != nil {
		return e
	}
	return ce
}
