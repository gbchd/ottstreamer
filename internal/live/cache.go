package live

import (
	"container/list"
	"fmt"
	"sync"
)

// segKey names one encoded segment set: every video rendition of segment n.
type segKey struct {
	stream string
	epoch  int
	n      int64
	inAd   bool
}

func (k segKey) String() string {
	return fmt.Sprintf("%s/%d/%d/%t", k.stream, k.epoch, k.n, k.inAd)
}

type cacheEntry struct {
	key  segKey
	set  map[string][]Sample
	size int64
}

// cache is an LRU of segment sets bounded by their sample bytes.
type cache struct {
	mu    sync.Mutex
	max   int64
	size  int64
	order *list.List // front = most recently used
	items map[segKey]*list.Element
}

func newCache(maxBytes int64) *cache {
	return &cache{max: maxBytes, order: list.New(), items: make(map[segKey]*list.Element)}
}

func (c *cache) get(k segKey) (map[string][]Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).set, true
}

func (c *cache) put(k segKey, set map[string][]Sample) {
	var size int64
	for _, samples := range set {
		for _, s := range samples {
			size += int64(len(s.Data))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.remove(el)
	}
	c.items[k] = c.order.PushFront(&cacheEntry{key: k, set: set, size: size})
	c.size += size
	for c.size > c.max {
		c.remove(c.order.Back())
	}
}

func (c *cache) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// purge removes every segment set of one epoch of a stream.
func (c *cache) purge(streamID string, epoch int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.items {
		if k.stream == streamID && k.epoch == epoch {
			c.remove(el)
		}
	}
}

func (c *cache) remove(el *list.Element) {
	e := c.order.Remove(el).(*cacheEntry)
	delete(c.items, e.key)
	c.size -= e.size
}
