package termui

import "unicode/utf8"

// Keys as a raw-mode read gives them, after Normalize.
const (
	KeyUp        = "\x1b[A"
	KeyDown      = "\x1b[B"
	KeyRight     = "\x1b[C"
	KeyLeft      = "\x1b[D"
	KeyHome      = "\x1b[H"
	KeyEnd       = "\x1b[F"
	KeyPgUp      = "\x1b[5~"
	KeyPgDn      = "\x1b[6~"
	KeyBackTab   = "\x1b[Z"
	KeyEnter     = "\r"
	KeyTab       = "\t"
	KeyEsc       = "\x1b"
	KeyBackspace = "\x7f"
	KeyCtrlC     = "\x03"
)

// SplitKeys cuts what one read gave into keys: escape sequences whole,
// a rune each otherwise (a paste gives many).
func SplitKeys(s string) []string {
	var out []string
	for s != "" {
		if s[0] == 0x1b && len(s) >= 3 && (s[1] == '[' || s[1] == 'O') {
			end := 2
			for end < len(s) && (s[end] < 0x40 || s[end] > 0x7e) {
				end++
			}
			if end < len(s) {
				end++
			}
			out = append(out, s[:end])
			s = s[end:]
			continue
		}
		_, size := utf8.DecodeRuneInString(s)
		out = append(out, s[:size])
		s = s[size:]
	}
	return out
}

// Normalize maps the variants terminals send to one key: SS3 arrows
// (application mode) to CSI, the alternate Home/End, newline to Enter,
// ^H to backspace.
func Normalize(k string) string {
	switch k {
	case "\x1bOA":
		return KeyUp
	case "\x1bOB":
		return KeyDown
	case "\x1bOC":
		return KeyRight
	case "\x1bOD":
		return KeyLeft
	case "\x1bOH", "\x1b[1~", "\x1b[7~":
		return KeyHome
	case "\x1bOF", "\x1b[4~", "\x1b[8~":
		return KeyEnd
	case "\n":
		return KeyEnter
	case "\b":
		return KeyBackspace
	}
	return k
}
