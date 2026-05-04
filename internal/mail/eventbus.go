package mail

import "sync"

type EventType string

const (
	EventNewMail     EventType = "new-mail"
	EventSyncComplete EventType = "sync-complete"
	EventSendResult  EventType = "send-result"
)

type Event struct {
	Type      EventType
	AccountID string
	FolderID  string
	Status    string
	Error     string
}

type EventBus struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[chan Event]struct{}),
	}
}

func (eb *EventBus) Subscribe() chan Event {
	ch := make(chan Event, 16)
	eb.mu.Lock()
	eb.subscribers[ch] = struct{}{}
	eb.mu.Unlock()
	return ch
}

func (eb *EventBus) Unsubscribe(ch chan Event) {
	eb.mu.Lock()
	delete(eb.subscribers, ch)
	eb.mu.Unlock()
}

func (eb *EventBus) Publish(event Event) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	for ch := range eb.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}
