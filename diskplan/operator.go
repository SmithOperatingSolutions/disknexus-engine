// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"fmt"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

// Operator-configured block-mode exclusions (#468).
//
// The built-in volatile list (pagefile & co) and the "don't back up the
// backup" paths are exclusions the engine decides. These are the ones an
// operator writes down: a scratch directory, a VM image folder, a cache —
// paths whose blocks are zeroed at capture exactly the way the volatile
// list's are (the same NTFS subtree walk, the same exclusion map, the same
// IsExcluded catalog marking, the same restore-as-zeros).
//
// The vocabulary is deliberately small: a drive-qualified path, meaning the
// file or the whole subtree at it. A trailing `\*` is accepted and means
// the same thing (people write it). No other wildcard — a pattern language
// that resolves against an NTFS MFT walk is a different feature, and an
// exclusion is a hole in a backup, so its meaning has to be obvious at a
// glance.
//
// Every exclusion gets an OUTCOME, because "I excluded it" and "I could not
// find it, so its bytes are in this backup" are opposite results that used
// to be equally silent. The caller reports them.

// Exclusion is one parsed operator exclusion.
type Exclusion struct {
	Raw   string // as the operator wrote it
	Drive string // "C:" — upper-cased, no trailing separator
	Rel   string // volume-root-relative, slash-separated, no leading slash: "Users/x/VMs"
}

// String renders the exclusion in its canonical Windows form.
func (e Exclusion) String() string {
	return e.Drive + `\` + strings.ReplaceAll(e.Rel, "/", `\`)
}

// ParseExclusion validates and normalizes one operator exclusion. It is the
// engine's half of the vocabulary — what a capture can resolve. Policy about
// which paths are foot-guns (\Windows, a whole users tree) belongs to the
// product's doors, on top of this.
func ParseExclusion(raw string) (Exclusion, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Exclusion{}, fmt.Errorf("exclusion is empty")
	}
	s = strings.ReplaceAll(s, "/", `\`)
	// Trailing `\*` means "everything under", which is what a path already
	// means here; accept it and drop it.
	s = strings.TrimSuffix(s, `\*`)
	if len(s) < 2 || s[1] != ':' || !isDriveLetter(s[0]) {
		return Exclusion{}, fmt.Errorf("exclusion %q must start with a drive letter (C:\\...)", raw)
	}
	if strings.ContainsAny(s, `*?<>"|`) {
		return Exclusion{}, fmt.Errorf("exclusion %q: wildcards are not supported — name the file or folder; a trailing \\* is allowed", raw)
	}
	drive := strings.ToUpper(s[:2])
	rel := strings.Trim(s[2:], `\`)
	if rel == "" {
		return Exclusion{}, fmt.Errorf("exclusion %q would exclude the whole volume", raw)
	}
	for _, seg := range strings.Split(rel, `\`) {
		if seg == "" || seg == "." || seg == ".." {
			return Exclusion{}, fmt.Errorf("exclusion %q: empty or relative path segment", raw)
		}
	}
	return Exclusion{Raw: raw, Drive: drive, Rel: strings.ReplaceAll(rel, `\`, "/")}, nil
}

func isDriveLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// ExclusionStatus is what became of one exclusion at capture.
type ExclusionStatus string

const (
	// ExclusionApplied: the path exists on the captured volume and its
	// blocks are zeroed in this capture.
	ExclusionApplied ExclusionStatus = "applied"
	// ExclusionNotOnVolume: the drive letter is not the captured volume's.
	// Nothing to do here; it may apply to another member of a disk capture.
	ExclusionNotOnVolume ExclusionStatus = "not-on-volume"
	// ExclusionNotFound: the volume is the right one, and the path is not
	// on it. ITS BYTES ARE NOT EXCLUDED — the operator must hear this.
	ExclusionNotFound ExclusionStatus = "not-found"
	// ExclusionUnsupported: the volume is not NTFS; the walk cannot resolve
	// paths there. Not excluded.
	ExclusionUnsupported ExclusionStatus = "unsupported-filesystem"
	// ExclusionFailed: the walk errored. Not excluded.
	ExclusionFailed ExclusionStatus = "failed"
)

// ExclusionOutcome reports one exclusion against one captured volume.
type ExclusionOutcome struct {
	Exclusion Exclusion
	Status    ExclusionStatus
	Bytes     int64  // distinct bytes this exclusion added to the map (applied only)
	Detail    string // filesystem name, or the error, for the non-applied cases
}

// Excluded says whether the exclusion's bytes are out of this capture.
func (o ExclusionOutcome) Excluded() bool { return o.Status == ExclusionApplied }

// Describe is the operator-facing one-liner.
func (o ExclusionOutcome) Describe() string {
	p := o.Exclusion.String()
	switch o.Status {
	case ExclusionApplied:
		return fmt.Sprintf("excluding %s (%s)", p, formatBytes(o.Bytes))
	case ExclusionNotOnVolume:
		return fmt.Sprintf("%s is not on this volume", p)
	case ExclusionNotFound:
		return fmt.Sprintf("WARNING: exclusion %s not found on the volume — its data is IN this backup", p)
	case ExclusionUnsupported:
		return fmt.Sprintf("WARNING: exclusion %s cannot be resolved on a %s volume (NTFS only) — its data is IN this backup", p, o.Detail)
	default:
		return fmt.Sprintf("WARNING: exclusion %s failed (%s) — its data is IN this backup", p, o.Detail)
	}
}

// ApplyExclusions resolves each exclusion against the volume being captured
// (scanSource: the VSS snapshot device, live volume, or image; volumeLetter:
// the drive it is, "C:" — empty when the capture has no letter, e.g. an
// image file, in which case every exclusion is taken to be for it) and adds
// the matching extents to m. One outcome per exclusion, in order.
func ApplyExclusions(scanSource, volumeLetter string, m *volume.ExclusionMap, excls []Exclusion) []ExclusionOutcome {
	out := make([]ExclusionOutcome, 0, len(excls))
	vl := strings.ToUpper(strings.TrimSuffix(volumeLetter, `\`))
	for _, e := range excls {
		o := ExclusionOutcome{Exclusion: e}
		if vl != "" && e.Drive != vl {
			o.Status = ExclusionNotOnVolume
			out = append(out, o)
			continue
		}
		before := m.CoveredBytes()
		res, err := volumefs.ExcludeSubtree(scanSource, e.Rel, m)
		switch {
		case err != nil:
			o.Status, o.Detail = ExclusionFailed, err.Error()
		case res.Filesystem != "ntfs":
			o.Status, o.Detail = ExclusionUnsupported, res.Filesystem
		case !res.Found:
			o.Status = ExclusionNotFound
		default:
			o.Status = ExclusionApplied
			o.Bytes = m.CoveredBytes() - before
		}
		out = append(out, o)
	}
	return out
}

// MemberExclusionRecord is what one captured volume's manifest records
// from its outcomes: the exclusions that were FOR this volume (its drive —
// not-on-volume ones belong to another member and are omitted, so a disk
// member's manifest never claims an exclusion meant for a different drive),
// and the warning line for each that did not apply.
func MemberExclusionRecord(outcomes []ExclusionOutcome) (paths, warnings []string) {
	for _, o := range outcomes {
		if o.Status == ExclusionNotOnVolume {
			continue
		}
		paths = append(paths, o.Exclusion.String())
		if !o.Excluded() {
			warnings = append(warnings, o.Describe())
		}
	}
	return paths, warnings
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
