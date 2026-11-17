package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Event is the immutable ledger record. Everything downstream reads these and
// nothing ever rewrites one.
type Event struct {
	Seq               int64
	EventID           string
	Source            string
	MessageType       string
	SourceCode        string
	ContractVersion   int
	IdempotencyKey    string
	DedupeKey         string
	BusinessKeys      map[string]string // canonical, by key kind
	RawBusinessKeys   map[string]string // as sent, by key kind
	PackageID         string            // raw package identifier as sent
	PackageKey        string
	PackageRevision   string
	ShipmentKey       string
	DeviceID          string
	DeviceEventNo     string
	EvidenceIndexes   []string
	OccurredAt        time.Time
	ReceivedAt        time.Time
	Payload           map[string]any
	ContentHash       string
	SupersedesEventID string
	MissingFields     []string
	UnmappedFields    []string
}

// Mode returns the operational mode carried by the payload, if any.
func (e *Event) Mode() string { return str(e.Payload, "mode") }

// EventIDOf returns the event's uuid.
func (e *Event) ID() string { return e.EventID }

// Stage is the merged-stage helper for this event.
func (e *Event) Stage() int { return EventStage(e.MessageType, e.Mode()) }

// Result is the screening verdict carried by a screening or review event.
func (e *Event) Result() string {
	switch e.MessageType {
	case TypeScreening:
		return stringsUpper(str(e.Payload, "result"))
	case TypeManualReview:
		return stringsUpper(str(e.Payload, "conclusion"))
	}
	return ""
}

func stringsUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// ContentFingerprint hashes only the semantically meaningful body, ignoring
// timestamps that legitimately vary between retransmissions. Two events with
// the same source+idempotency_key and equal fingerprints are true retransmits.
func ContentFingerprint(ev *EventEnvelope, packageKey, revision string) string {
	h := sha256.New()
	payload, _ := canonicalJSON(ev.Payload)
	fmt.Fprintf(h, "type=%s\ncode=%s\npkg=%s\nrev=%s\ndev=%s\ndevno=%s\nev=%s\npayload=%s\n",
		ev.MessageType, ev.SourceCode, packageKey, revision, ev.DeviceID, ev.DeviceEventNo,
		joinSorted(ev.EvidenceIndexes), payload)
	return hex.EncodeToString(h.Sum(nil))
}

// DedupeKey is the per-source primary deduplication identity. Sources that
// supply a device event number get device-scoped dedupe (the same physical
// device never reuses a number); everything else uses the explicit
// idempotency key. Content-level duplicates within a feed are caught
// separately by the duplicate-message rule using the fingerprint.
func DedupeKey(source, idempotencyKey, deviceID, deviceEventNo string) (string, string) {
	if deviceID != "" && deviceEventNo != "" {
		return "device:" + deviceID + "#" + deviceEventNo, "device"
	}
	if idempotencyKey != "" {
		return "idem:" + idempotencyKey, "idempotency_key"
	}
	return "", ""
}

func joinSorted(in []string) string {
	c := append([]string(nil), in...)
	sort.Strings(c)
	b, _ := json.Marshal(c)
	return string(b)
}

// canonicalJSON marshals with map keys sorted (encoding/json does) and no HTML
// escaping so the same payload always hashes the same.
func canonicalJSON(v any) (string, error) {
	var buf jsonBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(buf), nil
}

type jsonBuffer []byte

func (b *jsonBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}
