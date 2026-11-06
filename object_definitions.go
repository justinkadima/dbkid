package main

import "unicode/utf8"

// definitionJSONFits counts the encoding/json string representation without
// allocating an oversized escaped copy. used includes the response envelope and
// the separately accounted identifier values.
func definitionJSONFits(value []byte, used int) bool {
	used += 2 // quotes
	for i := 0; i < len(value); {
		c := value[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\n', '\r', '	':
				used += 2
			case '\b', '\f':
				// Go releases differ: reserve the longer \\u00xx form.
				used += 6
			case '<', '>', '&':
				used += 6
			default:
				if c < 0x20 {
					used += 6
				} else {
					used++
				}
			}
			i++
		} else {
			r, n := utf8.DecodeRune(value[i:])
			if r == utf8.RuneError && n == 1 {
				return false
			}
			if r == '\u2028' || r == '\u2029' {
				used += 6
			} else {
				used += n
			}
			i += n
		}
		if used > maxResultJSONBytes {
			return false
		}
	}
	return true
}
