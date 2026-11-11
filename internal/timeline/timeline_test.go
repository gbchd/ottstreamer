package timeline_test

import (
	"testing"
	"time"

	"ottstreamer/internal/timeline"
)

var ast = time.Date(2026, 11, 6, 18, 40, 0, 0, time.UTC)

// tl builds a timeline of segSeconds-long segments (given as frames at fps).
func tl(fps int, segFrames int64) timeline.Timeline {
	return timeline.Timeline{AST: ast, FPS: fps, SegFrames: segFrames, LeadSegs: 4, EndSeg: -1}
}

func at(d time.Duration) time.Time { return ast.Add(d) }

func TestTimeline_SegDuration(t *testing.T) {
	tests := []struct {
		name      string
		fps       int
		segFrames int64
		want      time.Duration
		ticks     int64
	}{
		{"2s at 25", 25, 50, 2 * time.Second, 180000},
		{"1.92s at 25", 25, 48, 1920 * time.Millisecond, 172800},
		{"1.6s at 30", 30, 48, 1600 * time.Millisecond, 144000},
		{"2s at 24", 24, 48, 2 * time.Second, 180000},
		{"0.5s at 60", 60, 30, 500 * time.Millisecond, 45000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tl(tt.fps, tt.segFrames)
			if got := l.SegDuration(); got != tt.want {
				t.Errorf("SegDuration = %v, want %v", got, tt.want)
			}
			if got := l.SegTicks(); got != tt.ticks {
				t.Errorf("SegTicks = %d, want %d", got, tt.ticks)
			}
		})
	}
}

func TestTimeline_SegsFor(t *testing.T) {
	l := tl(25, 48) // 1.92 s
	tests := []struct {
		d    time.Duration
		want int64
	}{
		{0, 0},
		{time.Second, 1},
		{1920 * time.Millisecond, 1},
		{8 * time.Second, 5},
		{30 * time.Second, 16},
	}
	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			if got := l.SegsFor(tt.d); got != tt.want {
				t.Errorf("SegsFor(%v) = %d, want %d", tt.d, got, tt.want)
			}
		})
	}
}

func TestTimeline_LiveEdgeAndFrozenHorizon(t *testing.T) {
	ended := tl(25, 50)
	ended.EndSeg = 10
	tests := []struct {
		name         string
		tl           timeline.Timeline
		now          time.Time
		edge, frozen int64
	}{
		{"long before AST", tl(25, 50), at(-time.Hour), -1, 3},
		{"just before AST", tl(25, 50), at(-time.Nanosecond), -1, 3},
		{"at AST", tl(25, 50), at(0), -1, 3},
		{"one tick before first segment ends", tl(25, 50), at(2*time.Second - 1), -1, 3},
		{"first segment ends", tl(25, 50), at(2 * time.Second), 0, 4},
		{"mid segment 5", tl(25, 50), at(11 * time.Second), 4, 8},
		{"fractional segments", tl(25, 48), at(1920 * time.Millisecond * 3), 2, 6},
		{"ended: horizon capped", ended, at(16 * time.Second), 7, 10},
		{"ended: edge capped", ended, at(time.Hour), 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tl.LiveEdge(tt.now); got != tt.edge {
				t.Errorf("LiveEdge = %d, want %d", got, tt.edge)
			}
			if got := tt.tl.FrozenHorizon(tt.now); got != tt.frozen {
				t.Errorf("FrozenHorizon = %d, want %d", got, tt.frozen)
			}
		})
	}
}

func TestTimeline_Available(t *testing.T) {
	l := tl(25, 50)
	now := at(100 * time.Second) // live edge 49
	retain := 10 * time.Second
	tests := []struct {
		name string
		n    int64
		want bool
	}{
		{"negative", -1, false},
		{"live edge", 49, true},
		{"not yet published", 50, false},
		{"exactly retain old", 44, true}, // ends at 90 s
		{"past retain", 43, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l.Available(tt.n, now, retain); got != tt.want {
				t.Errorf("Available(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestTimeline_SegStartEnd(t *testing.T) {
	l := tl(25, 48)
	if got, want := l.SegStart(10), at(19200*time.Millisecond); !got.Equal(want) {
		t.Errorf("SegStart(10) = %v, want %v", got, want)
	}
	if got, want := l.SegEnd(10), at(21120*time.Millisecond); !got.Equal(want) {
		t.Errorf("SegEnd(10) = %v, want %v", got, want)
	}
}

func TestTimeline_SegAtOrAfter(t *testing.T) {
	l := tl(25, 50)
	tests := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"before AST", at(-time.Minute), 0},
		{"at AST", at(0), 0},
		{"one tick after AST", at(1), 1},
		{"on boundary", at(10 * time.Second), 5},
		{"just after boundary", at(10*time.Second + time.Millisecond), 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l.SegAtOrAfter(tt.at); got != tt.want {
				t.Errorf("SegAtOrAfter = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTimeline_FirstFrameAndVideoTfdt(t *testing.T) {
	l := tl(25, 50)
	if got := l.FirstFrame(3); got != 150 {
		t.Errorf("FirstFrame(3) = %d, want 150", got)
	}
	if got := l.VideoTfdt(3); got != 540000 {
		t.Errorf("VideoTfdt(3) = %d, want 540000", got)
	}
}

func TestTimeline_AudioFrameRange(t *testing.T) {
	tests := []struct {
		name      string
		fps       int
		segFrames int64
		frameSize int
		counts    []int64 // frames in segments 0, 1, 2, ...
	}{
		{"2s AAC at 25", 25, 50, 1024, []int64{94, 94, 94, 93, 94}},
		{"2s AC-3 at 25", 25, 50, 1536, []int64{63, 62, 63, 62}},
		{"2s Opus at 25", 25, 50, 960, []int64{100, 100, 100}},
		{"1.92s AAC at 25 aligned", 25, 48, 1024, []int64{90, 90, 90, 90}},
		{"1.92s AC-3 at 25 aligned", 25, 48, 1536, []int64{60, 60, 60}},
		{"1.6s AAC at 30 aligned", 30, 48, 1024, []int64{75, 75, 75}},
		{"1.6s AC-3 at 30 aligned", 30, 48, 1536, []int64{50, 50}},
		{"2s AAC at 24", 24, 48, 1024, []int64{94, 94, 94, 93}},
		{"2s AAC at 30", 30, 60, 1024, []int64{94, 94, 94, 93}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tl(tt.fps, tt.segFrames)
			var wantFirst int64
			for n, want := range tt.counts {
				first, end := l.AudioFrameRange(int64(n), tt.frameSize)
				if first != wantFirst || end-first != want {
					t.Errorf("segment %d: [%d,%d), want first %d, %d frames", n, first, end, wantFirst, want)
				}
				wantFirst = end
			}
		})
	}
}

func TestTimeline_VideoSegAt(t *testing.T) {
	l := tl(25, 50) // 180000 ticks per segment
	tests := []struct {
		name   string
		ticks  int64
		want   int64
		wantOK bool
	}{
		{"zero", 0, 0, true},
		{"segment 7", 7 * 180000, 7, true},
		{"one tick late", 7*180000 + 1, 0, false},
		{"one tick early", 7*180000 - 1, 0, false},
		{"negative", -180000, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := l.VideoSegAt(tt.ticks)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("VideoSegAt(%d) = %d, %v, want %d, %v", tt.ticks, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestTimeline_AudioSegAt(t *testing.T) {
	l := tl(25, 50) // AAC: segment 1 starts at frame 94, segment 3 at frame 282
	tests := []struct {
		name   string
		ticks  int64
		want   int64
		wantOK bool
	}{
		{"zero", 0, 0, true},
		{"segment 1", 94 * 1024, 1, true},
		{"segment 3", 282 * 1024, 3, true},
		{"one tick late", 94*1024 + 1, 0, false},
		{"one tick early", 94*1024 - 1, 0, false},
		{"frame start inside a segment", 95 * 1024, 0, false},
		{"nominal boundary is not a frame start", 96000, 0, false},
		{"negative", -1024, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := l.AudioSegAt(tt.ticks, 1024)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("AudioSegAt(%d) = %d, %v, want %d, %v", tt.ticks, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
