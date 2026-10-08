package auth

import (
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// SetUserStorage configures owned lifecycle checks explicitly at managed
// application startup. Table presence never selects a layout. The coordinator
// must be the same one used by workers and handlers so deletion drains their
// operations. Managed startup configures it before registering routes.
func (m *Manager) SetUserStorage(routing *storage.AccountRouting) error {
	if routing == nil || routing.System() != m.db || m.config.AuthenticationMode() != ModeManaged {
		return errors.New("user storage requires managed authentication on the same system database")
	}
	if m.userStorage.CompareAndSwap(nil, routing) || m.userStorage.Load() == routing {
		return nil
	}
	return errors.New("authentication user storage coordinator is already configured")
}
