package wiki

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestCacheExpiresEntriesAtTheirOwnTTL(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	c := newCache(8, clk.now)
	c.put("hit", entry{text: "page"}, 6*time.Hour)
	c.put("miss", entry{miss: true}, 30*time.Minute)

	clk.t = clk.t.Add(31 * time.Minute)
	if _, ok := c.get("miss"); ok {
		t.Error("a miss outlived its 30 minute TTL")
	}
	if e, ok := c.get("hit"); !ok || e.text != "page" {
		t.Errorf("hit = %+v, %v; want the page still cached", e, ok)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	clk := &clock{t: time.Unix(0, 0)}
	c := newCache(2, clk.now)
	c.put("a", entry{text: "a"}, time.Hour)
	c.put("b", entry{text: "b"}, time.Hour)
	c.get("a")
	c.put("c", entry{text: "c"}, time.Hour)
	if _, ok := c.get("b"); ok {
		t.Error("b survived, but it was the least recently used")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.get(k); !ok {
			t.Errorf("%s was evicted", k)
		}
	}
}

func TestCacheIsSafeConcurrently(t *testing.T) {
	c := newCache(16, time.Now)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := strconv.Itoa(i % 20)
			c.put(k, entry{text: k}, time.Hour)
			c.get(k)
		}()
	}
	wg.Wait()
}
