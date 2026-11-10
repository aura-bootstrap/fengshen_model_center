package transcript

import (
	"path/filepath"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/subtitle"
)

func LoadSidecar(episode domain.Episode) (domain.Transcript, bool) {
	base := strings.TrimSuffix(episode.VideoPath, filepath.Ext(episode.VideoPath))
	for _, extension := range []string{".srt", ".vtt"} {
		path := base + extension
		result, err := subtitle.Parse(path, "sidecar:"+path)
		if err == nil && usable(result) {
			return result, true
		}
	}
	return domain.Transcript{}, false
}
