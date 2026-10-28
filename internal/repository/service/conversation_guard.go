package service

import "sync"

type ConversationGuard struct {
	mu     sync.Mutex
	active map[uint64]struct{}
}

func NewConversationGuard() *ConversationGuard {
	return &ConversationGuard{
		active: make(map[uint64]struct{}),
	}
}

func (g *ConversationGuard) TryAcquire(conversationID uint64) (release func(), acquired bool) {
	if conversationID == 0 {
		return nil, false
	}

	g.mu.Lock()

	if _, exists := g.active[conversationID]; exists {
		g.mu.Unlock()
		return nil, false
	}

	g.active[conversationID] = struct{}{}
	g.mu.Unlock()

	var once sync.Once

	release = func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.active, conversationID)
			g.mu.Unlock()
		})
	}

	return release, true
}
