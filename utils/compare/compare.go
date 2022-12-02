// Copyright (C) 2019-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package compare

// Returns true iff the slices have the same elements,
// regardless of order.
func UnsortedEquals[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[T]int, len(a))
	for i := 0; i < len(a); i++ {
		m[a[i]]++
	}
	for i := 0; i < len(b); i++ {
		v := b[i]
		switch count := m[v]; count {
		case 0:
			// There were more instances of [v] in [b] than [a].
			return false
		case 1:
			delete(m, v)
		default:
			m[v]--
		}
	}
	return len(m) == 0
}
