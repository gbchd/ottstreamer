package config_test

import (
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/config"
)

func TestLoad(t *testing.T) {
	env := map[string]string{"ORIGIN_LISTEN": ":9000", "ORIGIN_API_KEYS": "a:read:x, b:write:y", "ORIGIN_SHUTDOWN_DELAY": "5s", "ORIGIN_UI": "false"}
	c, err := config.Load([]string{"-listen", ":1", "-max-ffmpeg", "3", "-cors-api-origins", "https://ui.example", "-ffmpeg-version", "9.0.2"},
		func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.MaxFFmpeg != 3 || c.ProducerParallelism != 2 || c.ShutdownDelay != 5*time.Second || c.FFmpegVersion != "9.0.2" || c.UI ||
		!slices.Equal(c.APIKeys, []string{"a:read:x", "b:write:y"}) || !slices.Equal(c.CORSAPIOrigins, []string{"https://ui.example"}) {
		t.Errorf("Load = %+v", c)
	}

	if c, err := config.Load(nil, func(string) string { return "" }); err != nil || !c.UI {
		t.Errorf("default UI = %v, %v; want on", c.UI, err)
	}

	env = map[string]string{"ORIGIN_CACHE_BYTES": "lots"}
	if _, err := config.Load(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("bad ORIGIN_CACHE_BYTES accepted")
	}
}
