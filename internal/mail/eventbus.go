package mail

import "sync"

type EventType string

const (
	EventNewMail            EventType = "new-mail"
	EventSyncStarted        EventType = "sync-started"
	EventSyncProgress       EventType = "sync-progress"
	EventSyncComplete       EventType = "sync-complete"
	EventManualSyncStarted  EventType = "manual-sync-started"
	EventManualSyncProgress EventType = "manual-sync-progress"
	EventManualSyncComplete EventType = "manual-sync-complete"
	EventProcessingStatus   EventType = "processing-status"
	EventSendResult         EventType = "send-result"
	EventMutation           EventType = "mutation"
	EventAvatarUpdated      EventType = "avatar-updated"
	EventAvatarBackfill     EventType = "avatar-backfill"
	EventContactActivity    EventType = "contact-activity"
	EventContactBackfill    EventType = "contact-backfill"
)

type Event struct {
	Type          EventType
	AccountID     string
	FolderID      string
	FolderRole    string
	Payload       map[string]any
	Status        string
	Error         string
	Current       int
	Total         int
	AvatarHash    string
	AvatarURL     string
	AvatarDataURL string
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
