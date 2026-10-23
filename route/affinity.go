package route

import (
	"container/list"
	"sync"
)

// Affinity remembers which endpoint owns upstream state, such as a stored
// response or an uploaded file, so later requests that refer to it reach that
// endpoint first. Keys use the form of apix.RequestInfo.Affinity.
// Implementations must be safe for concurrent use; replace the default
// AffinityCache to share affinity across processes.
type Affinity interface {
	Lookup(key string) (endpointID string, ok bool)
	Remember(key, endpointID string)
}

// AffinityCache is the default Affinity: an in-process cache that keeps the
// Capacity most recently used keys (65536 when zero). The zero value is ready
// to use.
type AffinityCache struct {
	Capacity int

	mu    sync.Mutex
	order *list.List // front is most recent; values are *affinityEntry
	items map[string]*list.Element
}

type affinityEntry struct {
	key, endpointID string
}

func (c *AffinityCache) capacity() int {
	if c.Capacity < 1 {
		return 1 << 16
	}
	return c.Capacity
}

// Lookup returns the endpoint remembered for key.
func (c *AffinityCache) Lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*affinityEntry).endpointID, true
}

// Remember records the endpoint for key, evicting the least recently used
// key when the cache is full.
func (c *AffinityCache) Remember(key, endpointID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]*list.Element)
		c.order = list.New()
	}
	if el, ok := c.items[key]; ok {
		el.Value.(*affinityEntry).endpointID = endpointID
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&affinityEntry{key: key, endpointID: endpointID})
	for c.order.Len() > c.capacity() {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*affinityEntry).key)
	}
}
