package lgcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	"github.com/projanvil/langchain-golang/langgraph/checkpoint/serde"
)

// The fork's serde registry is deliberately closed (langchain core types
// plus JSON-native values). woca's interrupt payloads are harness structs
// (PermissionRequest and friends), so this file layers a woca-aware
// Serializer on top: values the fork registry handles pass through
// unchanged; types registered here (RegisterType) round-trip through a
// "woca:<name>" envelope. Unknown structs stay errors — no silent
// degradation.

// wocaTagPrefix namespaces registered-type tags inside the Serializer
// protocol ("woca:<name>").
const wocaTagPrefix = "woca:"

var (
	registerMu sync.RWMutex
	// registered maps a concrete type to its envelope name; newByName
	// holds zero values for decoding.
	registered = map[reflect.Type]string{}
	newByName  = map[string]any{}
)

// RegisterType registers a harness payload type for durable serialization.
// v must be a zero value of the type; name must be stable across releases
// (it is persisted). Registering the same (type, name) pair twice is a
// no-op; a conflicting re-registration panics.
func RegisterType(v any, name string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("lgcp: RegisterType wants a struct zero value, got %T", v))
	}
	if prev, ok := registered[t]; ok {
		if prev == name {
			return
		}
		panic(fmt.Sprintf("lgcp: type %s already registered as %q (want %q)", t, prev, name))
	}
	if _, taken := newByName[name]; taken {
		panic(fmt.Sprintf("lgcp: serializer name %q already in use", name))
	}
	registered[t] = name
	newByName[name] = v
}

// wocaSerializer extends the fork JSON serializer with the woca registry.
type wocaSerializer struct {
	base lgcheckpoint.Serializer
}

// newSerializer returns the serializer every saver in this package uses.
func newSerializer() lgcheckpoint.Serializer {
	return wocaSerializer{base: serde.NewJSONSerializer()}
}

// DumpsTyped implements lgcheckpoint.Serializer.
func (s wocaSerializer) DumpsTyped(v any) (string, []byte, error) {
	typ, data, err := s.base.DumpsTyped(v)
	if err == nil {
		return typ, data, nil
	}
	registerMu.RLock()
	name, ok := registered[reflect.TypeOf(v)]
	registerMu.RUnlock()
	if !ok {
		return "", nil, err // unknown type stays the fork's error
	}
	blob, merr := json.Marshal(v)
	if merr != nil {
		return "", nil, fmt.Errorf("lgcp: encode %s: %w", name, merr)
	}
	return wocaTagPrefix + name, blob, nil
}

// LoadsTyped implements lgcheckpoint.Serializer.
func (s wocaSerializer) LoadsTyped(typ string, data []byte) (any, error) {
	if name, ok := strings.CutPrefix(typ, wocaTagPrefix); ok {
		registerMu.RLock()
		zero, ok := newByName[name]
		registerMu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("lgcp: no type registered as %q", name)
		}
		out := reflect.New(reflect.TypeOf(zero)).Interface()
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("lgcp: decode %s: %w", name, err)
		}
		return reflect.ValueOf(out).Elem().Interface(), nil
	}
	return s.base.LoadsTyped(typ, data)
}
