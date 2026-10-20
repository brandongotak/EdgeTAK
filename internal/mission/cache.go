package mission

import (
	"container/list"
	"sync"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// latestCache remembers the most recent event per UID (bounded, LRU), so an
// item added to a mission by UID gets its CoT even if it was sent earlier,
// and clients can fetch it via /Marti/api/cot/xml/{uid}.
type latestCache struct {
	mu    sync.Mutex
	max   int
	order *list.List // of uid, most recent at front
	items map[string]*cacheItem
}

type cacheItem struct {
	ev     *cot.Event
	groups []string // groups of the sender, which bound who may read it
	elem   *list.Element
}

func newLatestCache(max int) *latestCache {
	return &latestCache{max: max, order: list.New(), items: map[string]*cacheItem{}}
}

func (c *latestCache) put(ev *cot.Event, groups []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it, ok := c.items[ev.UID]; ok {
		it.ev, it.groups = ev, groups
		c.order.MoveToFront(it.elem)
		return
	}
	c.items[ev.UID] = &cacheItem{ev: ev, groups: groups, elem: c.order.PushFront(ev.UID)}
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(string))
	}
}

func (c *latestCache) get(uid string) *cot.Event {
	ev, _ := c.lookup(uid)
	return ev
}

func (c *latestCache) lookup(uid string) (*cot.Event, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it, ok := c.items[uid]; ok {
		return it.ev, it.groups
	}
	return nil, nil
}
