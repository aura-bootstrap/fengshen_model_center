package audio

import "fmt"

// AnalysisOptions contains every public quality parameter for deterministic
// local signal analysis. PCM format, the 20 ms frame/hop, the 40 ms pitch
// observation window, and resource limits are deliberately not configurable.
//
// The zero value selects DefaultAnalysisOptions. A non-zero value is a complete
// configuration: start with DefaultAnalysisOptions when overriding only some
// fields, including when an override intentionally uses a valid zero value.
type AnalysisOptions struct {
	VAD     VADOptions
	Pitch   PitchOptions
	Overlap ConservativeOverlapDetector
}

// DefaultAnalysisOptions preserves the signal analyzer's historical defaults.
func DefaultAnalysisOptions() AnalysisOptions {
	return AnalysisOptions{
		VAD:     defaultVADOptions(),
		Pitch:   defaultPitchOptions(),
		Overlap: defaultConservativeOverlapDetector(),
	}
}

// WithDefaults resolves the backward-compatible zero value. Non-zero options
// are returned unchanged so valid explicit zero thresholds are not erased.
func (o AnalysisOptions) WithDefaults() AnalysisOptions {
	if o == (AnalysisOptions{}) {
		return DefaultAnalysisOptions()
	}
	return o
}

// Validate checks all public quality parameters and their cross-field
// relationships without inspecting or decoding media.
func (o AnalysisOptions) Validate() error {
	_, err := o.normalized()
	return err
}

func (o AnalysisOptions) normalized() (AnalysisOptions, error) {
	o = o.WithDefaults()
	// MaxFrames remains an internal resource bound on the legacy PitchOptions
	// API. It is not a public analysis-quality knob: zero selects the fixed
	// default and every other non-default value is rejected.
	if o.Pitch.MaxFrames == 0 {
		o.Pitch.MaxFrames = DefaultMaxFrames
	}
	if o.Pitch.MaxFrames != DefaultMaxFrames {
		return AnalysisOptions{}, fmt.Errorf("%w: pitch frame resource limit is fixed", ErrInvalidRequest)
	}
	vad, err := o.VAD.validated()
	if err != nil {
		return AnalysisOptions{}, err
	}
	pitch, err := o.Pitch.validated()
	if err != nil {
		return AnalysisOptions{}, err
	}
	overlap, err := o.Overlap.validated()
	if err != nil {
		return AnalysisOptions{}, err
	}
	o.VAD, o.Pitch, o.Overlap = vad, pitch, overlap
	return o, nil
}
