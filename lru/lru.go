/*
Copyright 2013 Google Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
     http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package lru implements an LRU cache.
package lru

import (
	"container/list"
	"sync"
	"time"
)

type (
	Key   string
	Value []byte
)

// Cache is an LRU cache. It is not safe for concurrent access.
type Cache struct {
	// MaxEntries is the maximum number of cache entries before
	// an item is evicted. Zero means no limit.
	MaxEntries int
	// TTL is the time-to-live of each entries in the cache.
	TTL time.Duration

	mu    sync.Mutex
	ll    *list.List
	cache map[Key]*list.Element
}

type entry struct {
	key   Key
	value Value
	date  time.Time
}

// New creates a new Cache.
// If maxEntries is zero, the cache has no limit and it's assumed
// that eviction is done by the caller.
func New(maxEntries int, ttl time.Duration) *Cache {
	return &Cache{
		MaxEntries: maxEntries,
		TTL:        ttl,
		ll:         list.New(),
		cache:      make(map[Key]*list.Element),
	}
}

// Add adds a value to the cache.
func (c *Cache) Add(key Key, value Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele, hit := c.cache[key]; hit {
		c.ll.MoveToFront(ele)
		ele.Value.(*entry).date = time.Now()
		ele.Value.(*entry).value = value
	} else {
		ele := c.ll.PushFront(&entry{key, value, time.Now()})
		c.cache[key] = ele
		if c.MaxEntries != 0 && c.ll.Len() > c.MaxEntries {
			c.RemoveOldest()
		}
	}
}

// Get looks up a key's value from the cache.
func (c *Cache) Get(key Key) (value Value, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele, hit := c.cache[key]; hit {
		if c.TTL == 0 || time.Since(ele.Value.(*entry).date) <= c.TTL {
			c.ll.MoveToFront(ele)
			ele.Value.(*entry).date = time.Now()
			return ele.Value.(*entry).value, true
		}
		c.removeElement(ele)
	}
	return
}

// Remove removes the provided key from the cache.
func (c *Cache) Remove(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele, hit := c.cache[key]; hit {
		c.removeElement(ele)
	}
}

// RemoveOldest removes the oldest item from the cache.
func (c *Cache) RemoveOldest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele := c.ll.Back(); ele != nil {
		c.removeElement(ele)
	}
}

func (c *Cache) removeElement(e *list.Element) {
	c.ll.Remove(e)
	kv := e.Value.(*entry)
	delete(c.cache, kv.key)
}
