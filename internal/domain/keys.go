package domain

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Business key kinds. Keys arrive from seven heterogeneous feeds with
// different encodings; everything is folded to a canonical form so identity
// resolution does not depend on a particular system's punctuation.
const (
	KeyShipment  = "shipment"
	KeyPackage   = "package"
	KeyContainer = "container"
	KeyLeg       = "leg"
	KeyAwb       = "awb"
)

var (
	awbRe       = regexp.MustCompile(`^([0-9]{3})-?([0-9]{4}|[0-9]{8})$`)
	legRe       = regexp.MustCompile(`^([A-Z0-9]{2,3})([0-9]{2,4})/?([0-9]{4}-[0-9]{2}-[0-9]{2})/?([A-Z]{3})-?([A-Z]{3})$`)
	containerRe = regexp.MustCompile(`^([A-Z]{3})([0-9]{4,8})([A-Z]{0,3})$`)
	simpleRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// NormalizeKey folds a raw identifier to the canonical business key and also
// reports whether the raw spelling already matched the canonical form. The
// boolean drives the "inconsistent source encoding" quality rule: a feed that
// sends "123-45678901" and another that sends "12345678901" are talking about
// the same shipment but encoding it differently, which must be visible.
func NormalizeKey(kind, raw string) (canonical string, alreadyCanonical bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, false
	}
	switch kind {
	case KeyAwb, KeyShipment:
		s := strings.ToUpper(raw)
		if m := awbRe.FindStringSubmatch(s); m != nil {
			c := "AWB" + m[1] + m[2]
			return c, s == raw && c == raw, true
		}
		if strings.HasPrefix(s, "AWB") {
			body := strings.TrimPrefix(s, "AWB")
			if m := awbRe.FindStringSubmatch(body); m != nil {
				c := "AWB" + m[1] + m[2]
				return c, c == raw, true
			}
			if simpleRe.MatchString(body) {
				return s, s == raw, true
			}
		}
		return "", false, false
	case KeyLeg:
		s := strings.ToUpper(strings.ReplaceAll(raw, " ", ""))
		if m := legRe.FindStringSubmatch(s); m != nil {
			c := m[1] + m[2] + "/" + m[3] + "/" + m[4] + "-" + m[5]
			return c, c == raw, true
		}
		return "", false, false
	case KeyContainer:
		s := strings.ToUpper(strings.ReplaceAll(raw, "-", ""))
		if m := containerRe.FindStringSubmatch(s); m != nil {
			c := m[1] + m[2] + m[3]
			return c, c == raw, true
		}
		return s, s == raw, simpleRe.MatchString(s)
	case KeyPackage:
		s := strings.ToUpper(strings.ReplaceAll(raw, " ", ""))
		return s, s == raw, simpleRe.MatchString(s)
	default:
		return raw, true, simpleRe.MatchString(raw)
	}
}

// CanonicalKeyOrRaw normalises and falls back to the trimmed raw value.
func CanonicalKeyOrRaw(kind, raw string) string {
	c, _, ok := NormalizeKey(kind, raw)
	if !ok {
		return raw
	}
	return c
}

// sanitizeForHash strips characters that vary without changing meaning so the
// content hash of two logically identical retransmissions is equal.
func sanitizeForHash(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// CanonicalPackageRevision folds revision labels ("Rev 2", "2", "v2") to "2".
func CanonicalPackageRevision(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "REV")
	s = strings.TrimPrefix(s, "V")
	s = strings.TrimSpace(s)
	return s
}

// SortedUnique returns a sorted copy without duplicates.
func SortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// EventStage is the operational stage an event belongs to, used by the
// time merger and by the status-regression rule.
func EventStage(messageType, mode string) int {
	switch messageType {
	case TypeDeclaration:
		return 10
	case TypePackageChange:
		return 15
	case TypeHandover:
		switch mode {
		case ModeScreening:
			return 25
		case ModeDeparture, ModeLoading:
			return 45
		default:
			return 20
		}
	case TypeScan:
		switch mode {
		case ModeArrival:
			return 30
		case ModeTransfer:
			return 35
		case ModeScreening:
			return 40
		case ModeLoading, ModeDeparture:
			return 50
		default:
			return 35
		}
	case TypeScreening:
		return 40
	case TypeEvidence:
		return 42
	case TypeManualReview:
		return 45
	case TypeContainerStow:
		return 50
	case TypeFlightManifest:
		return 60
	default:
		return 30
	}
}

var stageNames = map[int]string{
	10: "declared", 15: "package_revised", 20: "handover", 25: "handover_to_screening",
	30: "received", 35: "in_transit", 40: "screened", 42: "evidence_archived",
	45: "reviewed", 50: "loaded", 60: "manifested",
}

func StageName(stage int) string {
	if n, ok := stageNames[stage]; ok {
		return n
	}
	return "unknown"
}
