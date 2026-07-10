package session

import "strings"

// Labels are free-form strings on a session (set with `ax tag --add-label`). A
// label that contains "=" is treated as a key=value tag, which is the convention
// the picker uses for tag columns and group-by (e.g. "project=blog").
// Everything here is a pure function over a []string label set.

// LabelValue returns the value of the first key=value label matching key, or ""
// when none is present.
func LabelValue(labels []string, key string) string {
	for _, l := range labels {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// HasLabel reports whether a label set matches want: an exact label ("k=v" or a
// bare flag), or, given just a key, any k=... label with that key.
func HasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
		if !strings.Contains(want, "=") {
			if k, _, ok := strings.Cut(l, "="); ok && k == want {
				return true
			}
		}
	}
	return false
}

// LabelKeys returns the distinct key=value keys across sessions, in first-seen
// order. Bare (value-less) labels are skipped. Used to offer group-by dimensions.
func LabelKeys(sessions []Session) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		for _, l := range s.Labels {
			if k, _, ok := strings.Cut(l, "="); ok && k != "" && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// EditLabels applies one edit to a label set and returns the new set (unique,
// order preserved, input not mutated): "key=value" sets key (one value per key),
// "key=" or "-key" removes it, and a bare word adds a flag tag.
func EditLabels(labels []string, edit string) []string {
	edit = strings.TrimSpace(edit)
	if edit == "" {
		return labels
	}
	if rm := strings.TrimPrefix(edit, "-"); rm != edit {
		return dropLabel(labels, strings.TrimSpace(rm))
	}
	if k, v, ok := strings.Cut(edit, "="); ok {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		labels = dropLabelKey(labels, k)
		if v == "" {
			return labels // "key=" clears it
		}
		return append(labels, k+"="+v)
	}
	for _, l := range labels {
		if l == edit {
			return labels
		}
	}
	return append(labels, edit)
}

// dropLabel removes a label by exact match, or by key when token names a key.
func dropLabel(labels []string, token string) []string {
	out := labels[:0:0]
	for _, l := range labels {
		if l == token {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok && k == token {
			continue
		}
		out = append(out, l)
	}
	return out
}

// dropLabelKey removes any key=value label with the given key.
func dropLabelKey(labels []string, key string) []string {
	out := labels[:0:0]
	for _, l := range labels {
		if k, _, ok := strings.Cut(l, "="); ok && k == key {
			continue
		}
		out = append(out, l)
	}
	return out
}
