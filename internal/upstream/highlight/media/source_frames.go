package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

func sourceSeconds(v float64) string { return strconv.FormatFloat(v, 'f', 9, 64) }

// SourceFrameReader is optional; legacy Client implementations need not implement it.
type SourceFrameReader interface {
	SourceFrames(context.Context, string, float64, float64) (SourceFrameWindow, error)
}
type SourceFrame struct {
	PTS     int64
	Seconds float64
}
type SourceFrameWindow struct {
	Frames                   []SourceFrame
	TimeBaseNum, TimeBaseDen int64
	OriginSeconds            float64
	SourceVersion            string
}

// SourceVersion binds evidence to the same file identity used by selection catalogs.
func SourceVersion(path string) (string, error) {
	s, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !s.Mode().IsRegular() {
		return "", ErrInvalidRequest
	}
	return fmt.Sprintf("%d:%d", s.Size(), s.ModTime().UnixNano()), nil
}

func (c *FFmpegClient) SourceFrames(ctx context.Context, path string, lo, hi float64) (SourceFrameWindow, error) {
	var w SourceFrameWindow
	if !domain.ValidRange(lo, hi) || hi-lo > 602 {
		return w, ErrInvalidRequest
	}
	version, err := SourceVersion(path)
	if err != nil {
		return w, err
	}
	runner := c.commandRunner()
	if r, ok := runner.(ExecRunner); ok {
		r.RejectTruncation = true
		runner = r
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := runner.Run(ctx, c.ffprobePath(), "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=time_base,start_pts:format=start_time", "-of", "json", path)
	if err != nil {
		return w, err
	}
	var meta struct {
		Streams []struct {
			TimeBase string `json:"time_base"`
			StartPTS *int64 `json:"start_pts"`
		} `json:"streams"`
		Format struct {
			Start string `json:"start_time"`
		} `json:"format"`
	}
	if json.Unmarshal([]byte(out), &meta) != nil || len(meta.Streams) != 1 || meta.Streams[0].StartPTS == nil {
		return w, ErrInvalidRequest
	}
	parts := strings.Split(meta.Streams[0].TimeBase, "/")
	if len(parts) != 2 {
		return w, ErrInvalidRequest
	}
	n, e1 := strconv.ParseInt(parts[0], 10, 64)
	d, e2 := strconv.ParseInt(parts[1], 10, 64)
	origin, e3 := strconv.ParseFloat(meta.Format.Start, 64)
	if e1 != nil || e2 != nil || e3 != nil || n <= 0 || d <= 0 || !domain.Finite(origin) {
		return w, ErrInvalidRequest
	}
	// ffprobe seek may land on an earlier keyframe. Parse all returned frames,
	// validate continuity, then filter the exact requested episode coordinates.
	interval := sourceSeconds(math.Max(0, origin+lo-1)) + "%" + sourceSeconds(origin+hi+1)
	out, _, err = runner.Run(ctx, c.ffprobePath(), "-v", "error", "-select_streams", "v:0", "-read_intervals", interval, "-show_frames", "-show_entries", "frame=pts,duration,pkt_duration", "-of", "json", path)
	if err != nil {
		return w, err
	}
	frames, err := parseSourceFrames([]byte(out), lo, hi, n, d, origin)
	if err != nil {
		return w, err
	}
	after, err := SourceVersion(path)
	if err != nil || after != version {
		return w, ErrInvalidRequest
	}
	return SourceFrameWindow{Frames: frames, TimeBaseNum: n, TimeBaseDen: d, OriginSeconds: origin, SourceVersion: version}, nil
}

func parseSourceFrames(data []byte, lo, hi float64, n, d int64, origin float64) ([]SourceFrame, error) {
	var raw struct {
		Frames []struct {
			PTS            *int64 `json:"pts"`
			Duration       int64  `json:"duration"`
			PacketDuration int64  `json:"pkt_duration"`
		} `json:"frames"`
	}
	if json.Unmarshal(data, &raw) != nil || n <= 0 || d <= 0 || len(raw.Frames) < 2 {
		return nil, ErrInvalidRequest
	}
	result := []SourceFrame{}
	before, after := false, false
	for i, f := range raw.Frames {
		if f.PTS == nil || *f.PTS > 1<<52 || *f.PTS < -(1<<52) {
			return nil, ErrInvalidRequest
		}
		if i > 0 {
			prev := raw.Frames[i-1]
			dur := prev.Duration
			if dur == 0 {
				dur = prev.PacketDuration
			}
			if dur <= 0 || *f.PTS <= *prev.PTS || *f.PTS-*prev.PTS != dur {
				return nil, ErrInvalidRequest
			}
		}
		sec := float64(*f.PTS)*float64(n)/float64(d) - origin
		if sec <= lo {
			before = true
		}
		if sec >= hi {
			after = true
		}
		if sec >= lo && sec <= hi {
			result = append(result, SourceFrame{PTS: *f.PTS, Seconds: sec})
		}
	}
	if !before || !after || len(result) == 0 {
		return nil, ErrInvalidRequest
	}
	return result, nil
}

func (w SourceFrameWindow) Evidence(start, end SourceFrame) *domain.SourceCutEvidence {
	var pts []int64
	for _, f := range w.Frames {
		if f.PTS >= start.PTS && f.PTS <= end.PTS {
			pts = append(pts, f.PTS)
		}
	}
	data, _ := json.Marshal(pts)
	digest := sha256.Sum256(data)
	return &domain.SourceCutEvidence{Schema: 1, SourceVersion: w.SourceVersion, TimeBaseNum: w.TimeBaseNum, TimeBaseDen: w.TimeBaseDen, OriginSeconds: w.OriginSeconds, StartPTS: strconv.FormatInt(start.PTS, 10), EndPTS: strconv.FormatInt(end.PTS, 10), StartSeconds: start.Seconds, EndSeconds: end.Seconds, FrameSHA256: fmt.Sprintf("%x", digest)}
}

func (c *FFmpegClient) exportSourceCut(ctx context.Context, r ExportRequest) error {
	if err := c.validateSourceCut(ctx, r); err != nil {
		return fmt.Errorf("source cut evidence: %w", err)
	}
	e := r.SourceCut
	filter := fmt.Sprintf("settb=expr=%d/%d,trim=start_pts=%s:end_pts=%s,setpts=PTS-STARTPTS", e.TimeBaseNum, e.TimeBaseDen, e.StartPTS, e.EndPTS)
	_, _, err := c.commandRunner().Run(ctx, c.ffmpegPath(), "-copyts", "-i", r.SourcePath, "-ss", sourceSeconds(r.StartSec), "-t", sourceSeconds(r.EndSec-r.StartSec), "-i", r.SourcePath, "-map", "0:v:0", "-map", "1:a:0", "-vf", filter, "-af", "asetpts=PTS-STARTPTS", "-fps_mode", "passthrough", "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-c:a", "aac", "-b:a", "160k", "-y", r.OutputPath)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = os.Remove(r.OutputPath)
		return fmt.Errorf("export source frames: %w", err)
	}
	return nil
}

func (c *FFmpegClient) validateSourceCut(ctx context.Context, r ExportRequest) error {
	e := r.SourceCut
	if e == nil || e.Schema != 1 || e.SourceVersion == "" || r.SourceVersion != e.SourceVersion || r.StartSec != e.StartSeconds || r.EndSec != e.EndSeconds {
		return ErrInvalidRequest
	}
	w, err := c.SourceFrames(ctx, r.SourcePath, math.Max(0, r.StartSec-.001), r.EndSec+.001)
	if err != nil {
		return err
	}
	var a, b *SourceFrame
	for i := range w.Frames {
		f := &w.Frames[i]
		if strconv.FormatInt(f.PTS, 10) == e.StartPTS {
			a = f
		}
		if strconv.FormatInt(f.PTS, 10) == e.EndPTS {
			b = f
		}
	}
	if a == nil || b == nil {
		return ErrInvalidRequest
	}
	actual := w.Evidence(*a, *b)
	x, _ := json.Marshal(actual)
	y, _ := json.Marshal(e)
	if string(x) != string(y) {
		return ErrInvalidRequest
	}
	return nil
}
