// Package facehaar implements the shared pure-Go OpenCV Haar face detector.
// Callers supply the exact frontal and profile XML selected by their binding;
// this package neither discovers files nor substitutes embedded models.
package facehaar

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
)

const (
	MaxImageBytes = 8 << 20
	MaxDimension  = 4096
)

var (
	ErrInvalidImage  = errors.New("image must be a bounded PNG or JPEG")
	ErrImageTooLarge = errors.New("image exceeds the byte limit")
	ErrUnavailable   = errors.New("Haar detector is unavailable")
)

type Kind string

const (
	Frontal Kind = "frontal"
	Profile Kind = "profile"
	NoFace  Kind = "no_face"
)

type Result struct {
	Kind Kind `json:"kind"`
}

type faceCascade interface {
	detect(context.Context, *image.Gray) (bool, error)
}

// Detector is immutable after construction and may be used concurrently.
type Detector struct{ frontal, profile faceCascade }

// New parses both bound models. It retains parsed values, not the caller's bytes.
func New(frontalXML, profileXML []byte) (*Detector, error) {
	f, err := parseHaarCascade(frontalXML)
	if err != nil {
		return nil, err
	}
	p, err := parseHaarCascade(profileXML)
	if err != nil {
		return nil, err
	}
	return &Detector{frontal: f, profile: p}, nil
}

// Detect checks encoded size and header dimensions before allocating decoded pixels.
// Cancellation is cooperative; the standard image decoder itself is not interruptible.
func (d *Detector) Detect(ctx context.Context, data []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d == nil || d.frontal == nil || d.profile == nil {
		return Result{}, ErrUnavailable
	}
	if len(data) > MaxImageBytes {
		return Result{}, ErrImageTooLarge
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || !bounded(cfg.Width, cfg.Height) {
		return Result{}, ErrInvalidImage
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	decoded, actual, err := image.Decode(bytes.NewReader(data))
	if err != nil || actual != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return Result{}, ErrInvalidImage
	}
	return d.DetectImage(ctx, decoded)
}

func bounded(w, h int) bool { return w > 0 && h > 0 && w <= MaxDimension && h <= MaxDimension }

// DetectImage runs the same classifier on an already decoded image. The caller is
// responsible for bounding decoding/allocation. This preserves the server's
// independent, existing upload limits rather than applying runtime upload limits.
func (d *Detector) DetectImage(ctx context.Context, source image.Image) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d == nil || d.frontal == nil || d.profile == nil {
		return Result{}, ErrUnavailable
	}
	if source == nil || source.Bounds().Empty() {
		return Result{}, ErrInvalidImage
	}
	gray := detectionGray(source)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	frontal, err := d.frontal.detect(ctx, gray)
	if err != nil {
		return Result{}, err
	}
	if frontal {
		return checkedResult(ctx, Frontal)
	}
	profile, err := d.profile.detect(ctx, gray)
	if err != nil {
		return Result{}, err
	}
	if !profile {
		profile, err = d.profile.detect(ctx, flipGray(gray))
		if err != nil {
			return Result{}, err
		}
	}
	if profile {
		return checkedResult(ctx, Profile)
	}
	return checkedResult(ctx, NoFace)
}
func checkedResult(ctx context.Context, kind Kind) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Kind: kind}, nil
}
