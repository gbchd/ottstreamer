package live

import "testing"

func set(bytes int) map[string][]Sample {
	return map[string][]Sample{"720p": {{Data: make([]byte, bytes)}}}
}

func TestCache_evictsLeastRecentlyUsedByBytes(t *testing.T) {
	c := newCache(250)
	k := func(n int64) segKey { return segKey{stream: "a", epoch: 1, n: n} }
	c.put(k(0), set(100))
	c.put(k(1), set(100))
	c.get(k(0)) // 1 is now the least recently used
	c.put(k(2), set(100))

	for n, want := range []bool{true, false, true} {
		if _, ok := c.get(k(int64(n))); ok != want {
			t.Errorf("segment %d cached = %t, want %t", n, ok, want)
		}
	}
	if c.size != 200 {
		t.Errorf("size = %d, want 200", c.size)
	}
}

func TestCache_purge(t *testing.T) {
	c := newCache(1000)
	c.put(segKey{stream: "a", epoch: 1, n: 0}, set(10))
	c.put(segKey{stream: "a", epoch: 1, n: 0, inAd: true}, set(10))
	c.put(segKey{stream: "a", epoch: 2, n: 0}, set(10))
	c.put(segKey{stream: "b", epoch: 1, n: 0}, set(10))
	c.purge("a", 1)
	if len(c.items) != 2 || c.size != 20 {
		t.Fatalf("%d items, %d bytes left; want 2, 20", len(c.items), c.size)
	}
}
