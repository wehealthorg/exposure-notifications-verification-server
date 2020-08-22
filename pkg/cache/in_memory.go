// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"context"
	"sync"
	"time"
)

var _ Cacher = (*inMemory)(nil)

// inMemory is an in-memory cache implementation. It's good for local
// development and testing, but isn't recommended in production as the caches
// aren't shared among instances.
type inMemory struct {
	prefix string
	data   map[string]*item
	mu     sync.RWMutex

	stopCh chan struct{}
}

type item struct {
	value   interface{}
	expires int64
}

type InMemoryConfig struct {
	// Prefix is a custom value to prefix keys in shared systems.
	Prefix string

	// GCInterval is how frequently to purge stale entries from the cache.
	GCInterval time.Duration
}

// NewInMemory creates a new in-memory cache.
func NewInMemory(i *InMemoryConfig) (Cacher, error) {
	if i == nil {
		i = new(InMemoryConfig)
	}

	gcInterval := 4 * time.Hour
	if i.GCInterval > 0 {
		gcInterval = i.GCInterval
	}

	c := &inMemory{
		prefix: i.Prefix,
		data:   make(map[string]*item),
		stopCh: make(chan struct{}),
	}
	go c.cleanup(gcInterval)

	return c, nil
}

// Fetch attempts to retrieve the given key from the cache. If successful, it
// returns the value. If the value does not exist, it calls f and caches the
// result of f in the cache for ttl. The ttl is calculated from the time the
// value is inserted, not the time the function is called.
func (c *inMemory) Fetch(_ context.Context, key string, out interface{}, ttl time.Duration, f FetchFunc) error {
	now := time.Now().UnixNano()

	if c.prefix != "" {
		key = c.prefix + key
	}

	// Try a read-only lock first
	c.mu.RLock()
	if c.data == nil {
		c.mu.RUnlock()
		return ErrStopped
	}

	if i, ok := c.data[key]; ok && now < i.expires {
		c.mu.RUnlock()
		return readInto(i.value, out)
	}
	c.mu.RUnlock()

	// Now acquire a full lock, it's possible another goroutine wrote between our
	// read and write lock.
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data == nil {
		return ErrStopped
	}

	if i, ok := c.data[key]; ok && now < i.expires {
		return readInto(i.value, out)
	}

	// The value is not in the cache (or the value exists but has expired), call f
	// to get a new value.
	if f == nil {
		return ErrMissingFetchFunc
	}

	val, err := f()
	if err != nil {
		return err
	}

	if err := readInto(val, out); err != nil {
		return err
	}

	c.data[key] = &item{
		value: val,
		// Explicitly re-caputure the time instead of using now.
		expires: time.Now().UnixNano() + int64(ttl),
	}

	return nil
}

// Write adds a new item to the cache with the given TTL.
func (c *inMemory) Write(_ context.Context, key string, value interface{}, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data == nil {
		return ErrStopped
	}

	if c.prefix != "" {
		key = c.prefix + key
	}

	c.data[key] = &item{
		value:   value,
		expires: time.Now().UnixNano() + int64(ttl),
	}
	return nil
}

// Read fetches the value at the key. If the value does not exist, it returns
// ErrNotFound. If the types are incompatible, it returns an error.
func (c *inMemory) Read(_ context.Context, key string, out interface{}) error {
	now := time.Now().UnixNano()

	c.mu.RLock()
	if c.data == nil {
		c.mu.RUnlock()
		return ErrStopped
	}

	if c.prefix != "" {
		key = c.prefix + key
	}

	if i, ok := c.data[key]; ok {
		// Item is still valid
		if now < i.expires {
			c.mu.RUnlock()
			return readInto(i.value, out)
		}

		// Item has expired, defer deletion (we don't have an exclusive lock)
		go c.purge(key, i.expires)
	}

	c.mu.RUnlock()
	return ErrNotFound
}

// Delete removes an item from the cache, if it exists, regardless of TTL.
func (c *inMemory) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data == nil {
		return ErrStopped
	}

	if c.prefix != "" {
		key = c.prefix + key
	}

	delete(c.data, key)
	return nil
}

// Close completely stops the cacher. It is not safe to use after closing.
func (c *inMemory) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data != nil {
		close(c.stopCh)
	}
	c.data = nil

	return nil
}

// purge removes an item by key in the cache. If the item does not exist, it
// does nothing. If the item exists, but the expected expiration time is
// different, it does nothing. The expected expiration time is used to handle a
// race where the item is updated by another routine.
func (c *inMemory) purge(key string, expectedTTL int64) {
	c.mu.Lock()
	if c.data != nil {
		if i, ok := c.data[key]; ok && i.expires == expectedTTL {
			delete(c.data, key)
		}
	}
	c.mu.Unlock()
}

// cleanup deletes stale entries from the cache. Read operations are already a
// write-through cache, so this is run infrequently. It's designed to catch very
// old stale values that are no longer used.
func (c *inMemory) cleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}

		now := time.Now().UnixNano()

		c.mu.Lock()
		for k, i := range c.data {
			if i.expires < now {
				delete(c.data, k)
			}
		}
		c.mu.Unlock()
	}
}
