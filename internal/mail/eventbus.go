package mail

import "sync"

type EventType string

const (
	EventNewMail               EventType = "new-mail"
	EventSyncStarted           EventType = "sync-started"
	EventSyncProgress          EventType = "sync-progress"
	EventSyncComplete          EventType = "sync-complete"
	EventAccountSyncStatus     EventType = "account-sync-status"
	EventIDLEFolderStatus      EventType = "idle-folder-status"
	EventManualSyncStarted     EventType = "manual-sync-started"
	EventManualSyncProgress    EventType = "manual-sync-progress"
	EventManualSyncComplete    EventType = "manual-sync-complete"
	EventScheduledSyncStarted  EventType = "scheduled-sync-started"
	EventScheduledSyncProgress EventType = "scheduled-sync-progress"
	EventScheduledSyncComplete EventType = "scheduled-sync-complete"
	EventProcessingStatus      EventType = "processing-status"
	EventSendResult            EventType = "send-result"
	EventMutation              EventType = "mutation"
	EventAvatarUpdated         EventType = "avatar-updated"
	EventAvatarBackfill        EventType = "avatar-backfill"
	EventContactActivity       EventType = "contact-activity"
	EventContactBackfill       EventType = "contact-backfill"
	EventCalendarSync          EventType = "calendar-sync"
	EventCalendarChanged       EventType = "calendar-changed"
	EventCalendarReminder      EventType = "calendar-reminder"
	// EventMailRuleRun tells how a rule run by hand is going.
	EventMailRuleRun EventType = "mail-rule-run"
)

type Event struct {
	Type          EventType
	AccountID     string
	UserID        string
	UserIDs       []string
	AdminOnly     bool
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

// EventBus delivers events to the browsers and workers that want them.
// Browser connections subscribe for one user (SubscribeFor), so an event
// reaches only the connections of the people it is about rather than every
// connection on the server; each connection still checks what it may see.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
	all         map[*Subscription]struct{}
	users       map[string]map[*Subscription]struct{}
	accounts    map[string]map[*Subscription]struct{}
	anyAccount  map[*Subscription]struct{}
	admins      map[*Subscription]struct{}
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[chan Event]struct{}),
		all:         make(map[*Subscription]struct{}),
		users:       make(map[string]map[*Subscription]struct{}),
		accounts:    make(map[string]map[*Subscription]struct{}),
		anyAccount:  make(map[*Subscription]struct{}),
		admins:      make(map[*Subscription]struct{}),
	}
}

// Subscribe returns a channel of every event. A full channel misses events.
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

// SubscribeOptions chooses the events a Subscription receives. An event that
// names users goes to those users; otherwise one that names an account goes
// to subscribers of that account and to AnyAccount subscribers; otherwise an
// administrator event goes to administrators.
type SubscribeOptions struct {
	UserID     string
	Admin      bool
	AccountIDs []string
	// AnyAccount receives events naming only an account, for subscribers
	// that check ownership themselves.
	AnyAccount bool
	// All receives every event, for workers.
	All bool
	// Types, when set, limits the events to these types.
	Types []EventType
}

// SubscribeFor returns a subscription for the events opts selects. Close it
// when done.
func (eb *EventBus) SubscribeFor(opts SubscribeOptions) *Subscription {
	sub := &Subscription{bus: eb, opts: opts, ready: make(chan struct{}, 1), pending: make(map[progressKey]int)}
	if len(opts.Types) > 0 {
		sub.types = make(map[EventType]bool, len(opts.Types))
		for _, t := range opts.Types {
			sub.types[t] = true
		}
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	if opts.All {
		eb.all[sub] = struct{}{}
		return sub
	}
	if opts.UserID != "" {
		addSubscriber(eb.users, opts.UserID, sub)
	}
	for _, id := range opts.AccountIDs {
		addSubscriber(eb.accounts, id, sub)
	}
	if opts.AnyAccount {
		eb.anyAccount[sub] = struct{}{}
	}
	if opts.Admin {
		eb.admins[sub] = struct{}{}
	}
	return sub
}

func addSubscriber(index map[string]map[*Subscription]struct{}, key string, sub *Subscription) {
	set := index[key]
	if set == nil {
		set = make(map[*Subscription]struct{})
		index[key] = set
	}
	set[sub] = struct{}{}
}

func removeSubscriber(index map[string]map[*Subscription]struct{}, key string, sub *Subscription) {
	if set := index[key]; set != nil {
		delete(set, sub)
		if len(set) == 0 {
			delete(index, key)
		}
	}
}

func (eb *EventBus) unsubscribe(sub *Subscription) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	delete(eb.all, sub)
	if sub.opts.UserID != "" {
		removeSubscriber(eb.users, sub.opts.UserID, sub)
	}
	for _, id := range sub.opts.AccountIDs {
		removeSubscriber(eb.accounts, id, sub)
	}
	delete(eb.anyAccount, sub)
	delete(eb.admins, sub)
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
	for sub := range eb.all {
		sub.deliver(event)
	}
	switch users := eventUsers(event); {
	case len(users) == 1:
		for sub := range eb.users[users[0]] {
			sub.deliver(event)
		}
	case len(users) > 1:
		seen := make(map[string]bool, len(users))
		for _, user := range users {
			if seen[user] {
				continue
			}
			seen[user] = true
			for sub := range eb.users[user] {
				sub.deliver(event)
			}
		}
	case event.AccountID != "":
		for sub := range eb.accounts[event.AccountID] {
			sub.deliver(event)
		}
		for sub := range eb.anyAccount {
			sub.deliver(event)
		}
	case event.AdminOnly:
		for sub := range eb.admins {
			sub.deliver(event)
		}
	}
}

// eventUsers returns the users an event is limited to. Every user field
// limits it, so the most specific one set is enough to route it.
func eventUsers(event Event) []string {
	if event.UserID != "" {
		return []string{event.UserID}
	}
	if len(event.UserIDs) > 0 {
		return event.UserIDs
	}
	if user, _ := event.Payload["user_id"].(string); user != "" {
		return []string{user}
	}
	return nil
}

// subscriptionQueueLimit bounds the events waiting for a slow subscriber;
// later events are missed until it catches up.
const subscriptionQueueLimit = 256

type progressKey struct{ user, account, folder string }

// Subscription queues the events selected for one subscriber. Progress of a
// folder sync that is still waiting is replaced by newer progress, so a busy
// sync cannot crowd out new mail or changes.
type Subscription struct {
	bus   *EventBus
	opts  SubscribeOptions
	types map[EventType]bool
	ready chan struct{}

	mu      sync.Mutex
	queue   []Event
	pending map[progressKey]int // waiting sync-progress event by folder
	closed  bool
}

// Ready receives when events are waiting; Take them.
func (s *Subscription) Ready() <-chan struct{} { return s.ready }

// Take returns the waiting events in order.
func (s *Subscription) Take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.queue
	s.queue = nil
	clear(s.pending)
	// A signal left for events taken here would wake the reader for nothing.
	select {
	case <-s.ready:
	default:
	}
	return events
}

// Close stops delivery.
func (s *Subscription) Close() {
	s.bus.unsubscribe(s)
	s.mu.Lock()
	s.closed = true
	s.queue = nil
	s.mu.Unlock()
}

func (s *Subscription) deliver(event Event) {
	if s.types != nil && !s.types[event.Type] {
		return
	}
	key := progressKey{event.UserID, event.AccountID, event.FolderID}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if event.Type == EventSyncProgress && event.FolderID != "" {
		if i, ok := s.pending[key]; ok {
			s.queue[i] = event
			s.mu.Unlock()
			return
		}
	} else if event.FolderID != "" {
		// Progress queued before this event must not take a later value.
		delete(s.pending, key)
	}
	if len(s.queue) >= subscriptionQueueLimit {
		s.mu.Unlock()
		return
	}
	if event.Type == EventSyncProgress && event.FolderID != "" {
		s.pending[key] = len(s.queue)
	}
	s.queue = append(s.queue, event)
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}
