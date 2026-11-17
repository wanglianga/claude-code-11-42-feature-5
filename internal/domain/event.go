// Package domain holds the wire contract, the event model and the pure
// decision logic used by the security-status engine. Nothing here touches the
// database so every rule can be unit-tested and replayed deterministically.
package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// EventEnvelope is what every source posts to /v1/events. It carries the four
// business dimensions the station needs to reconcile: who sent it (source +
// source_code), how to deduplicate it (idempotency_key), when it happened vs
// when it arrived (occurred_at / received_at), and which physical or digital
// object it is about (business keys + package revision + evidence).
type EventEnvelope struct {
	MessageType       string            `json:"message_type"`
	SourceCode        string            `json:"source_code"`
	IdempotencyKey    string            `json:"idempotency_key"`
	OccurredAt        time.Time         `json:"occurred_at"`
	ReceivedAt        *time.Time        `json:"received_at,omitempty"`
	DeviceID          string            `json:"device_id,omitempty"`
	DeviceEventNo     string            `json:"device_event_no,omitempty"`
	PackageID         string            `json:"package_id,omitempty"`
	PackageRevision   string            `json:"package_revision,omitempty"`
	BusinessKeys      map[string]string `json:"business_keys,omitempty"`
	EvidenceIndexes   []string          `json:"evidence_indexes,omitempty"`
	SupersedesEventID string            `json:"supersedes_event_id,omitempty"`
	Payload           json.RawMessage   `json:"payload"`
}

// Message types the platform understands. The source column of a contract is
// the coarse feed; message_type is the semantic kind of every row.
const (
	TypeDeclaration    = "declaration"
	TypeScan           = "scan"
	TypeScreening      = "screening"
	TypeManualReview   = "manual_review"
	TypeContainerStow  = "container_stow"
	TypeFlightManifest = "flight_manifest"
	TypePackageChange  = "package_change"
	TypeHandover       = "handover"
	// TypeEvidence is emitted by the image index library: it registers that a
	// device event's image/CT slices are archived under an evidence index.
	// Separating it from the screening verdict is what makes "verdict exists but
	// evidence index is missing" observable across sources.
	TypeEvidence = "evidence"

	ModeArrival   = "arrival"
	ModeTransfer  = "transfer"
	ModeDeparture = "departure"
	ModeScreening = "screening"
	ModeLoading   = "loading"
)

var messageTypes = map[string]bool{
	TypeDeclaration: true, TypeScan: true, TypeScreening: true, TypeManualReview: true,
	TypeContainerStow: true, TypeFlightManifest: true, TypePackageChange: true,
	TypeHandover: true, TypeEvidence: true,
}

func IsKnownMessageType(t string) bool { return messageTypes[t] }

// FieldKind is the type a contract can require from a payload field.
type FieldKind string

const (
	KindString  FieldKind = "string"
	KindNumber  FieldKind = "number"
	KindBoolean FieldKind = "boolean"
	KindObject  FieldKind = "object"
	KindArray   FieldKind = "array"
	KindEnum    FieldKind = "enum"
	KindTime    FieldKind = "timestamp"
)

// FieldSpec describes one payload field in a contract.
type FieldSpec struct {
	Kind     FieldKind `json:"kind"`
	Required bool      `json:"required"`
	Enum     []string  `json:"enum,omitempty"`
}

// ContractDefinition is the schema registered for a (source, message_type,
// version). policy decides how payloads that carry extra or unknown fields are
// treated: strict rejects them, backward accepts them and records the drift,
// free stores them untouched.
type ContractDefinition struct {
	Fields []FieldSpecBinding `json:"fields"`
	Policy string             `json:"policy"` // strict|backward|free
}

type FieldSpecBinding struct {
	Name string    `json:"name"`
	Spec FieldSpec `json:"spec"`
}

// ValidationResult is the outcome of checking a payload against a contract.
type ValidationResult struct {
	OK             bool     `json:"ok"`
	MissingFields  []string `json:"missing_fields"`
	UnmappedFields []string `json:"unmapped_fields"`
	Errors         []string `json:"errors"`
}

var timeLayouts = []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"}

// ParseTime accepts RFC3339 with or without fractional seconds.
func ParseTime(s string) (time.Time, error) {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func ValidBusinessKey(s string) bool { return keyPattern.MatchString(s) }

// ValidateContractDefinition checks the contract itself before it is stored,
// so a malformed contract can never accept events.
func ValidateContractDefinition(def ContractDefinition) error {
	switch def.Policy {
	case "strict", "backward", "free":
	default:
		return fmt.Errorf("policy must be strict, backward or free")
	}
	if len(def.Fields) == 0 {
		return fmt.Errorf("contract must declare at least one field")
	}
	seen := map[string]bool{}
	for _, f := range def.Fields {
		if !ValidBusinessKey(f.Name) {
			return fmt.Errorf("invalid field name %q", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Spec.Kind {
		case KindString, KindNumber, KindBoolean, KindObject, KindArray, KindTime:
		case KindEnum:
			if len(f.Spec.Enum) == 0 {
				return fmt.Errorf("enum field %q needs at least one value", f.Name)
			}
		default:
			return fmt.Errorf("field %q has unknown kind %q", f.Name, f.Spec.Kind)
		}
	}
	return nil
}

// ValidatePayload applies a contract to a raw payload and reports missing
// required fields, unmapped fields and type/enum errors. A missing required
// field does not reject the event at intake (late corrections are normal in
// cargo); it makes the event "incomplete evidence" and downstream rules turn
// that into a data status. Type errors do reject.
func ValidatePayload(def ContractDefinition, payload map[string]any) ValidationResult {
	res := ValidationResult{OK: true}
	declared := map[string]bool{}
	for _, f := range def.Fields {
		declared[f.Name] = true
		v, present := payload[f.Name]
		if !present || v == nil {
			if f.Spec.Required {
				res.MissingFields = append(res.MissingFields, f.Name)
			}
			continue
		}
		if err := checkKind(f.Spec, v); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", f.Name, err))
		}
	}
	for name := range payload {
		if !declared[name] {
			res.UnmappedFields = append(res.UnmappedFields, name)
		}
	}
	sort.Strings(res.MissingFields)
	sort.Strings(res.UnmappedFields)
	if len(res.MissingFields) > 0 {
		res.OK = false
	}
	if len(res.Errors) > 0 {
		res.OK = false
	}
	if def.Policy == "strict" && len(res.UnmappedFields) > 0 {
		res.OK = false
		res.Errors = append(res.Errors, "strict contract rejects unmapped fields: "+strings.Join(res.UnmappedFields, ","))
	}
	return res
}

func checkKind(spec FieldSpec, v any) error {
	switch spec.Kind {
	case KindString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case KindNumber:
		switch v.(type) {
		case float64, int, int64, json.Number:
		default:
			return fmt.Errorf("expected number")
		}
	case KindBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	case KindObject:
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("expected object")
		}
	case KindArray:
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("expected array")
		}
	case KindTime:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected timestamp string")
		}
		if _, err := ParseTime(s); err != nil {
			return err
		}
	case KindEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected enum string")
		}
		ok = false
		for _, e := range spec.Enum {
			if e == s {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("value %q not in enum", s)
		}
	}
	return nil
}

// Typed accessors. Payloads are stored as JSON so accessors are permissive and
// centralise the casting rules.

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func num(p map[string]any, k string) (float64, bool) {
	switch v := p[k].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func boolOf(p map[string]any, k string) (bool, bool) {
	b, ok := p[k].(bool)
	return b, ok
}

func strs(p map[string]any, k string) []string {
	switch arr := p[k].(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), arr...)
	}
	return nil
}
