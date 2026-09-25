package wiki

import (
	"container/list"
	"sync"
	"time"
)

// entry is one cached lookup. A miss is cached as well as a hit, so a
// misspelled topic asked over and over costs the wiki one search, not one
// per question.
type entry struct {
	text string
	miss bool
}

type cached struct {
	key     string
	value   entry
	expires time.Time
}

// cache is a size-bounded LRU with a TTL per entry. Per entry because hits
// and misses age differently: a page changes rarely, while a miss is often a
// page someone is about to create.
type cache struct {
	mu    sync.Mutex
	size  int
	now   func() time.Time
	order *list.List
	items map[string]*list.Element
}

func newCache(size int, now func() time.Time) *cache {
	return &cache{size: size, now: now, order: list.New(), items: make(map[string]*list.Element, size)}
}

func (c *cache) get(key string) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return entry{}, false
	}
	item := el.Value.(*cached)
	if !c.now().Before(item.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		return entry{}, false
	}
	c.order.MoveToFront(el)
	return item.value, true
}

func (c *cache) put(key string, e entry, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		item := el.Value.(*cached)
		item.value, item.expires = e, c.now().Add(ttl)
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&cached{key: key, value: e, expires: c.now().Add(ttl)})
	for c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cached).key)
	}
}
