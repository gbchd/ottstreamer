package config_test

import (
	"slices"
	"testing"

	"ottstreamer/internal/config"
)

func TestLoad(t *testing.T) {
	env := map[string]string{"ORIGIN_LISTEN": ":9000", "ORIGIN_API_KEYS": "a:read:x, b:write:y"}
	c, err := config.Load([]string{"-listen", ":1", "-max-ffmpeg", "3", "-cors-api-origins", "https://ui.example"},
		func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.MaxFFmpeg != 3 || c.ProducerParallelism != 2 ||
		!slices.Equal(c.APIKeys, []string{"a:read:x", "b:write:y"}) || !slices.Equal(c.CORSAPIOrigins, []string{"https://ui.example"}) {
		t.Errorf("Load = %+v", c)
	}

	env = map[string]string{"ORIGIN_CACHE_BYTES": "lots"}
	if _, err := config.Load(nil, func(k string) string { return env[k] }); err == nil {
		t.Error("bad ORIGIN_CACHE_BYTES accepted")
	}
}
