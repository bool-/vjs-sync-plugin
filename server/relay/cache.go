package relay

import (
	"container/list"
	"context"
	"sync"
)

type cacheKey struct {
	room  string
	gen   uint64
	level int
	seq   int // -1 for the poster
}

// cache is an LRU of segments with a byte budget, and it deduplicates
// fetches: however many viewers ask for a segment at once, one upstream
// request is made and they all get its result. Errors are not cached.
type cache struct {
	mu      sync.Mutex
	budget  int64
	used    int64
	order   *list.List // front = most recent
	entries map[cacheKey]*list.Element
	flights map[cacheKey]*flight
}

type entry struct {
	key  cacheKey
	data []byte
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

func newCache(budget int64) *cache {
	return &cache{budget: budget, order: list.New(), entries: map[cacheKey]*list.Element{}, flights: map[cacheKey]*flight{}}
}

func (c *cache) get(ctx context.Context, k cacheKey, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.entries[k]; ok {
		c.order.MoveToFront(el)
		data := el.Value.(*entry).data
		c.mu.Unlock()
		return data, nil
	}
	f, ok := c.flights[k]
	if !ok {
		f = &flight{done: make(chan struct{})}
		c.flights[k] = f
		go func() {
			data, err := load()
			c.mu.Lock()
			f.data, f.err = data, err
			delete(c.flights, k)
			if err == nil {
				c.putLocked(k, data)
			}
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.data, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *cache) putLocked(k cacheKey, data []byte) {
	size := int64(len(data))
	if size > c.budget {
		return
	}
	c.entries[k] = c.order.PushFront(&entry{key: k, data: data})
	c.used += size
	for c.used > c.budget {
		el := c.order.Back()
		e := el.Value.(*entry)
		c.order.Remove(el)
		delete(c.entries, e.key)
		c.used -= int64(len(e.data))
	}
}

// dropRoom forgets a room's entries for every generation other than keep.
func (c *cache) dropRoom(room string, keep uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.entries {
		if k.room == room && k.gen != keep {
			c.order.Remove(el)
			delete(c.entries, k)
			c.used -= int64(len(el.Value.(*entry).data))
		}
	}
}

func (c *cache) size() (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used, len(c.entries)
}
