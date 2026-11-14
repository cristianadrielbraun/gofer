package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

// Kind is what a setting is about, which decides who may change it and how
// the console groups it.
type Kind string

const (
	KindSecurity   Kind = "security"
	KindCapability Kind = "capability"
	KindLimit      Kind = "limit"
	KindPrivacy    Kind = "privacy"
	// KindPreference is a default people can change for themselves, unless
	// a policy locks it.
	KindPreference Kind = "preference"
)

// Category is the tab of the policy editor a setting is on.
type Category string

const (
	CategorySignIn      Category = "sign_in"
	CategoryMail        Category = "mail"
	CategoryData        Category = "data"
	CategoryPreferences Category = "preferences"
	CategoryActivity    Category = "activity"
)

// Spec describes a setting, whatever its type.
type Spec struct {
	// Key names it in storage, the API and the audit log, such as
	// "mail.own_proxies". It never changes once released.
	Key      string
	Category Category
	Kind     Kind
	// InstanceOnly settings only make sense for the whole server: only the
	// Default policy sets them.
	InstanceOnly bool
	// Label and Help are translation keys.
	Label string
	Help  string
}

// Setting is a declared setting: its spec, its built-in default, and how
// its values are checked.
type Setting struct {
	Spec
	builtin any
	decode  func(json.RawMessage) (any, error)
	// adapter reads the Default policy's value from where the setting
	// lived before policies; nil for a setting policies store themselves.
	adapter Adapter
	// Min and Max bound a number, and Max a list's length; Options are an
	// enum's choices.
	Min, Max int
	Options  []string
}

// Adapter reads a setting's Default policy value from where it lived
// before policies, so it doesn't move: the stored value, and whether no
// other policy may change it. ok is false when nothing is stored there.
type Adapter func(ctx context.Context, q Querier) (raw json.RawMessage, enforced, ok bool, err error)

// Adapted reports whether the setting's Default policy value lives
// elsewhere: it's changed where it lives, not by saving it to the Default
// policy.
func (setting *Setting) Adapted() bool { return setting.adapter != nil }

// adapt gives setting its adapter.
func (setting *Setting) adapt(adapter Adapter) *Setting {
	setting.adapter = adapter
	return setting
}

// Default is the value the setting has when no policy sets it.
func (setting *Setting) Default() any { return setting.builtin }

// Decode reads a stored value, refusing one of the wrong type or outside
// the setting's bounds.
func (setting *Setting) Decode(raw json.RawMessage) (any, error) {
	return setting.decode(raw)
}

// Encode turns a value into what's stored, checking it the same way. A
// list is stored cleaned, sorted and without repeats.
func (setting *Setting) Encode(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrValueInvalid, setting.Key, err)
	}
	return setting.canonical(raw)
}

// canonical is raw as it's stored: checked, then written again from what
// it says.
func (setting *Setting) canonical(raw json.RawMessage) (json.RawMessage, error) {
	decoded, err := setting.decode(raw)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrValueInvalid, setting.Key, err)
	}
	return canonical, nil
}

func invalid(key, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrValueInvalid, key, fmt.Sprintf(format, args...))
}

// Bool declares an on/off setting.
func Bool(spec Spec, builtin bool) *Setting { return register(boolSetting(spec, builtin)) }

// Int declares a whole number from min to max.
func Int(spec Spec, builtin, min, max int) *Setting {
	return register(intSetting(spec, builtin, min, max))
}

// Enum declares a choice of one of options.
func Enum(spec Spec, builtin string, options ...string) *Setting {
	return register(enumSetting(spec, builtin, options...))
}

// StringList declares a list of entries, each cleaned by clean, which
// refuses one it doesn't take, and at most max of them. Nothing is listed
// by default.
func StringList(spec Spec, clean func(string) (string, error), max int) *Setting {
	return register(listSetting(spec, clean, max))
}

func listSetting(spec Spec, clean func(string) (string, error), max int) *Setting {
	setting := &Setting{Spec: spec, builtin: []string{}, Max: max}
	setting.decode = func(raw json.RawMessage) (any, error) {
		var entries []string
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, invalid(spec.Key, "not a list")
		}
		cleaned := make([]string, 0, len(entries))
		for _, entry := range entries {
			value, err := clean(entry)
			if err != nil {
				return nil, invalid(spec.Key, "%q: %v", entry, err)
			}
			cleaned = append(cleaned, value)
		}
		sort.Strings(cleaned)
		cleaned = slices.Compact(cleaned)
		if len(cleaned) > max {
			return nil, invalid(spec.Key, "more than %d entries", max)
		}
		return cleaned, nil
	}
	return setting
}

func boolSetting(spec Spec, builtin bool) *Setting {
	setting := &Setting{Spec: spec, builtin: builtin}
	setting.decode = func(raw json.RawMessage) (any, error) {
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, invalid(spec.Key, "not on or off")
		}
		return value, nil
	}
	return setting
}

func intSetting(spec Spec, builtin, min, max int) *Setting {
	if builtin < min || builtin > max {
		panic(fmt.Sprintf("policy: %s: default %d outside %d–%d", spec.Key, builtin, min, max))
	}
	setting := &Setting{Spec: spec, builtin: builtin, Min: min, Max: max}
	setting.decode = func(raw json.RawMessage) (any, error) {
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, invalid(spec.Key, "not a whole number")
		}
		if value < min || value > max {
			return nil, invalid(spec.Key, "%d outside %d–%d", value, min, max)
		}
		return value, nil
	}
	return setting
}

func enumSetting(spec Spec, builtin string, options ...string) *Setting {
	if !slices.Contains(options, builtin) {
		panic(fmt.Sprintf("policy: %s: default %q isn't an option", spec.Key, builtin))
	}
	setting := &Setting{Spec: spec, builtin: builtin, Options: slices.Clone(options)}
	setting.decode = func(raw json.RawMessage) (any, error) {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || !slices.Contains(options, value) {
			return nil, invalid(spec.Key, "not one of the choices")
		}
		return value, nil
	}
	return setting
}

var (
	registryMu sync.RWMutex
	registry   = map[string]*Setting{}
)

// ErrUnknownSetting is a key no setting is declared with.
var ErrUnknownSetting = errors.New("no setting has that key")

func register(setting *Setting) *Setting {
	if !validKey(setting.Key) || setting.Category == "" || setting.Kind == "" || setting.Label == "" {
		panic(fmt.Sprintf("policy: incomplete setting %q", setting.Key))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, taken := registry[setting.Key]; taken {
		panic(fmt.Sprintf("policy: setting %q declared twice", setting.Key))
	}
	registry[setting.Key] = setting
	return setting
}

// Lookup returns the setting declared with key.
func Lookup(key string) (*Setting, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	setting, ok := registry[key]
	return setting, ok
}

// Settings lists every declared setting by key.
func Settings() []*Setting {
	registryMu.RLock()
	defer registryMu.RUnlock()
	settings := make([]*Setting, 0, len(registry))
	for _, setting := range registry {
		settings = append(settings, setting)
	}
	sort.Slice(settings, func(i, j int) bool { return settings[i].Key < settings[j].Key })
	return settings
}
