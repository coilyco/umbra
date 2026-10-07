package audit

import (
	"strconv"
	"strings"
)

// Filter returns the records keep accepts, in order. Rows from before
// Record.Version existed have an empty Version and match no version selector.
func Filter(records []Record, keep func(Record) bool) []Record {
	var out []Record
	for _, r := range records {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// ByVersion selects the rows stamped with exactly version.
func ByVersion(version string) func(Record) bool {
	return func(r Record) bool { return r.Version == version }
}

// AtOrAfter selects rows from a build at or after release, a tag like "v0.30.0".
// A non-semver Version ("dev", "(devel)", empty) cannot be placed, so never matches.
func AtOrAfter(release string) func(Record) bool {
	want, ok := parseSemver(release)
	return func(r Record) bool {
		if !ok {
			return false
		}
		got, gok := parseSemver(r.Version)
		return gok && got.compare(want) >= 0
	}
}

type semver struct {
	nums [3]uint64
	pre  string
}

// parseSemver reads vMAJOR.MINOR.PATCH with an optional prerelease and build
// metadata suffix. The leading v is required, matching Go module versions.
func parseSemver(s string) (semver, bool) {
	var v semver
	if !strings.HasPrefix(s, "v") {
		return v, false
	}
	s = s[1:]
	s, _, _ = strings.Cut(s, "+")
	s, v.pre, _ = strings.Cut(s, "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, false
		}
		v.nums[i] = n
	}
	return v, true
}

func (a semver) compare(b semver) int {
	for i := range a.nums {
		switch {
		case a.nums[i] < b.nums[i]:
			return -1
		case a.nums[i] > b.nums[i]:
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	return strings.Compare(a.pre, b.pre)
}
