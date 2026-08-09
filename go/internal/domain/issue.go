package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"
)

var ErrInvalidIssue = errors.New("invalid_issue")

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

const maxJSONDepth = 128

type BlockerRef struct {
	ID         *string `json:"id"`
	Identifier *string `json:"identifier"`
	State      *string `json:"state"`
}

type Issue struct {
	ID           string         `json:"id"`
	NativeRef    map[string]any `json:"native_ref"`
	Identifier   string         `json:"identifier"`
	Title        string         `json:"title"`
	Description  *string        `json:"description"`
	Priority     *int           `json:"priority"`
	State        string         `json:"state"`
	BranchName   *string        `json:"branch_name"`
	URL          *string        `json:"url"`
	AssigneeID   *string        `json:"assignee_id"`
	Labels       []string       `json:"labels"`
	BlockedBy    []BlockerRef   `json:"blocked_by"`
	Dispatchable bool           `json:"dispatchable"`
	CreatedAt    *time.Time     `json:"created_at"`
	UpdatedAt    *time.Time     `json:"updated_at"`
}

// ValidateRequired verifies the provider-neutral fields that every adapter
// must produce. Dispatchable is intentionally not inferred: false is a valid,
// explicit provider decision.
func (issue Issue) ValidateRequired() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "id", value: issue.ID},
		{name: "identifier", value: issue.Identifier},
		{name: "title", value: issue.Title},
		{name: "state", value: issue.State},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidIssue, field.name)
		}
	}
	if _, err := cloneNativeRef(issue.NativeRef); err != nil {
		return fmt.Errorf("%w: native_ref: %v", ErrInvalidIssue, err)
	}
	return nil
}

// Clone returns a deep copy suitable for an immutable snapshot. It rejects a
// native_ref that cannot be represented as JSON; adapter normalization may
// instead choose the specification's null fallback before taking a snapshot.
func (issue Issue) Clone() (Issue, error) {
	clone := issue
	nativeRef, err := cloneNativeRef(issue.NativeRef)
	if err != nil {
		return Issue{}, fmt.Errorf("%w: native_ref: %v", ErrInvalidIssue, err)
	}
	clone.NativeRef = nativeRef
	clone.Description = cloneString(issue.Description)
	clone.Priority = cloneInt(issue.Priority)
	clone.BranchName = cloneString(issue.BranchName)
	clone.URL = cloneString(issue.URL)
	clone.AssigneeID = cloneString(issue.AssigneeID)
	clone.Labels = cloneStrings(issue.Labels)
	clone.BlockedBy = cloneBlockers(issue.BlockedBy)
	clone.CreatedAt = cloneTime(issue.CreatedAt)
	clone.UpdatedAt = cloneTime(issue.UpdatedAt)
	return clone, nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneBlockers(blockers []BlockerRef) []BlockerRef {
	if blockers == nil {
		return nil
	}
	clone := make([]BlockerRef, len(blockers))
	for index, blocker := range blockers {
		clone[index] = BlockerRef{
			ID:         cloneString(blocker.ID),
			Identifier: cloneString(blocker.Identifier),
			State:      cloneString(blocker.State),
		}
	}
	return clone
}

func cloneNativeRef(nativeRef map[string]any) (map[string]any, error) {
	if nativeRef == nil {
		return nil, nil
	}
	clone, err := cloneJSONValue(reflect.ValueOf(nativeRef), make(map[jsonVisit]struct{}), 0)
	if err != nil {
		return nil, err
	}
	return clone.Interface().(map[string]any), nil
}

type jsonVisit struct {
	typeOf  reflect.Type
	kind    reflect.Kind
	pointer uintptr
}

func cloneJSONValue(value reflect.Value, stack map[jsonVisit]struct{}, depth int) (reflect.Value, error) {
	if depth > maxJSONDepth {
		return reflect.Value{}, errors.New("value is too deeply nested")
	}
	if !value.IsValid() {
		return reflect.Value{}, nil
	}
	if value.Type() == reflect.TypeFor[json.Number]() {
		if !jsonNumberPattern.MatchString(string(value.Interface().(json.Number))) {
			return reflect.Value{}, errors.New("invalid JSON number")
		}
		return value, nil
	}
	if value.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := value.Interface().(json.RawMessage)
		if !json.Valid(raw) {
			return reflect.Value{}, errors.New("invalid raw JSON")
		}
		clone := append(json.RawMessage(nil), raw...)
		return reflect.ValueOf(clone), nil
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		return cloneJSONValue(value.Elem(), stack, depth+1)
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value, nil
	case reflect.Float32, reflect.Float64:
		if math.IsNaN(value.Float()) || math.IsInf(value.Float(), 0) {
			return reflect.Value{}, errors.New("non-finite number")
		}
		return value, nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, fmt.Errorf("map key type %s is not a string", value.Type().Key())
		}
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		visit := jsonVisit{typeOf: value.Type(), kind: value.Kind(), pointer: value.Pointer()}
		if _, found := stack[visit]; found {
			return reflect.Value{}, errors.New("cyclic value")
		}
		stack[visit] = struct{}{}
		defer delete(stack, visit)

		clone := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			item, err := cloneJSONValue(iterator.Value(), stack, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			item, err = assignableJSONValue(item, value.Type().Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			clone.SetMapIndex(iterator.Key(), item)
		}
		return clone, nil
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		visit := jsonVisit{typeOf: value.Type(), kind: value.Kind(), pointer: value.Pointer()}
		if _, found := stack[visit]; found {
			return reflect.Value{}, errors.New("cyclic value")
		}
		stack[visit] = struct{}{}
		defer delete(stack, visit)

		clone := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			item, err := cloneJSONValue(value.Index(index), stack, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			item, err = assignableJSONValue(item, value.Type().Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Index(index).Set(item)
		}
		return clone, nil
	case reflect.Array:
		clone := reflect.New(value.Type()).Elem()
		for index := range value.Len() {
			item, err := cloneJSONValue(value.Index(index), stack, depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			item, err = assignableJSONValue(item, value.Type().Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Index(index).Set(item)
		}
		return clone, nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported JSON value type %s", value.Type())
	}
}

func assignableJSONValue(value reflect.Value, destination reflect.Type) (reflect.Value, error) {
	if !value.IsValid() {
		if destination.Kind() == reflect.Interface {
			return reflect.Zero(destination), nil
		}
		return reflect.Value{}, fmt.Errorf("null is not assignable to %s", destination)
	}
	if value.Type().AssignableTo(destination) {
		return value, nil
	}
	if destination.Kind() == reflect.Interface && value.Type().Implements(destination) {
		return value, nil
	}
	return reflect.Value{}, fmt.Errorf("%s is not assignable to %s", value.Type(), destination)
}
