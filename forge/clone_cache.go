package main

import (
	"container/list"
	"sync"
)

const (
	maxShallowReplies = 8
	maxShallowBytes   = 4 << 20
)

type shallowReply struct {
	tip  Oid
	body []byte
}

type shallowBuild struct {
	done chan struct{}
	body []byte
}

// shallowReplyCache shares the immutable framed reply for a depth-one clone
// across connections. Callers hold the store read lock throughout generation.
// Concurrent misses for the same commit join one build; unrelated tips can
// be compressed in parallel.
type shallowReplyCache struct {
	mu       sync.Mutex
	byTip    map[Oid]*list.Element
	building map[Oid]*shallowBuild
	lru      list.List
	bytes    int
}

func (c *shallowReplyCache) get(tip Oid, build func() []byte) []byte {
	c.mu.Lock()
	if cached := c.byTip[tip]; cached != nil {
		c.lru.MoveToFront(cached)
		body := cached.Value.(shallowReply).body
		c.mu.Unlock()
		return body
	}
	if active := c.building[tip]; active != nil {
		c.mu.Unlock()
		<-active.done
		return active.body
	}
	if c.building == nil {
		c.building = make(map[Oid]*shallowBuild)
	}
	active := &shallowBuild{done: make(chan struct{})}
	c.building[tip] = active
	c.mu.Unlock()

	body := build()
	c.mu.Lock()
	c.retain(tip, body)
	delete(c.building, tip)
	active.body = body
	close(active.done)
	c.mu.Unlock()
	return body
}

func (c *shallowReplyCache) retain(tip Oid, body []byte) {
	if cap(body) > maxShallowBytes {
		return
	}
	for c.lru.Len() >= maxShallowReplies || c.bytes+cap(body) > maxShallowBytes {
		last := c.lru.Back()
		reply := last.Value.(shallowReply)
		delete(c.byTip, reply.tip)
		c.bytes -= cap(reply.body)
		c.lru.Remove(last)
	}
	if c.byTip == nil {
		c.byTip = make(map[Oid]*list.Element)
	}
	c.byTip[tip] = c.lru.PushFront(shallowReply{tip: tip, body: body})
	c.bytes += cap(body)
}

// clear runs under the exclusive store lock. Replies already handed to an
// HTTP connection remain valid until that connection finishes writing them.
func (c *shallowReplyCache) clear() {
	if c.byTip != nil {
		c.byTip = nil
		c.lru.Init()
		c.bytes = 0
	}
}
