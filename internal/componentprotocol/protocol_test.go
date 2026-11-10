package componentprotocol

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictJSONRejects(t *testing.T) {
	var v map[string]any
	cases := []string{
		``,
		`{"a":1,"a":2}`,
		`{"a":1} trailing`,
		`{"a":{"b":1,"b":2}}`,
	}
	for _, raw := range cases {
		if err := StrictJSON([]byte(raw), &v); err == nil {
			t.Fatalf("accepted invalid JSON: %q", raw)
		}
	}
}

func TestDecodeStream(t *testing.T) {
	model := quote(filepath.Join(t.TempDir(), "m.bin"))
	base := `{"protocol":"fengshen.stream.ws/v1","type":"start","request_id":"r1","episode_number":1,"duration_sec":1.5,"format":"s16le/16000/1","models":{"whisper-asr":` + model + `}}`
	if _, err := DecodeStream([]byte(base)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(base, `"type":"start"`, `"type":"audio"`, 1),
		strings.Replace(base, `fengshen.stream.ws/v1`, `other`, 1),
		strings.Replace(base, `"duration_sec":1.5`, `"duration_sec":0`, 1),
		strings.Replace(base, `"models":{`, `"models":{"extra":"x",`, 1),
		strings.Replace(base, `"models":{`, `"extra":1,"models":{`, 1),
	} {
		if _, err := DecodeStream([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid stream message: %s", raw)
		}
	}
	audio := `{"protocol":"fengshen.stream.ws/v1","type":"audio","request_id":"r1","episode_number":1,"revision":1,"start_sec":0,"end_sec":2,"sample_count":32000,"pcm":"AAAA"}`
	if _, err := DecodeStream([]byte(audio)); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJobRequest(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "a.wav")
	model := filepath.Join(t.TempDir(), "m.bin")
	ok := `{"protocol":"fengshen.batch.http/v1","kind":"whisper-batch","request_id":"r1","input":{"audio_path":` + quote(audio) + `},"models":{"whisper-asr":[` + quote(model) + `]},"options":{"vad":false}}`
	req, err := DecodeJobRequest([]byte(ok))
	if err != nil || req.Kind != "whisper-batch" {
		t.Fatalf("%v %+v", err, req)
	}
	bad := []string{
		strings.Replace(ok, `fengshen.batch.http/v1`, `fengshen.stream.ws/v1`, 1),
		strings.Replace(ok, `whisper-batch`, `demucs`, 1),
		strings.Replace(ok, quote(audio), `"relative.wav"`, 1),
		strings.Replace(ok, `"vad":false`, `"vad":false,"extra":1`, 1),
	}
	for _, raw := range bad {
		if _, err := DecodeJobRequest([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid job: %s", raw)
		}
	}
}

func TestArtifactWhitelist(t *testing.T) {
	if !ArtifactAllowed("whisper-batch", "transcript.json") || ArtifactAllowed("whisper-batch", "../x") {
		t.Fatal("whisper artifact whitelist broken")
	}
	if !ArtifactAllowed("seedvc", "converted.wav") || ArtifactAllowed("seedvc", "vocals.wav") {
		t.Fatal("seedvc artifact whitelist broken")
	}
	if len(Artifacts("demucs")) != 2 {
		t.Fatal("demucs artifact list broken")
	}
}

func TestValidAbsPath(t *testing.T) {
	if ValidAbsPath("") || ValidAbsPath("a") || ValidAbsPath(`C:\`) {
		t.Fatal("accepted invalid path")
	}
	dir := t.TempDir()
	if !ValidAbsPath(filepath.Join(dir, "x")) {
		t.Fatal("rejected valid path")
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
