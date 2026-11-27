package keywords

import "strings"

// FlagColor is the RFC 9979 bit pattern. Zero has no separate label.
type FlagColor int

const (
	FlagColorOrange FlagColor = 1 + iota
	FlagColorYellow
	FlagColorGreen
	FlagColorBlue
	FlagColorPurple
	FlagColorGray
)

// FlagColors lists the supported label colors in RFC order.
func FlagColors() []FlagColor {
	return []FlagColor{FlagColorOrange, FlagColorYellow, FlagColorGreen, FlagColorBlue, FlagColorPurple, FlagColorGray}
}

// Valid reports whether color has a visible label.
func (color FlagColor) Valid() bool {
	return color >= FlagColorOrange && color <= FlagColorGray
}

// Key gives the stable, untranslated name of a visible color.
func (color FlagColor) Key() string {
	if !color.Valid() {
		return ""
	}
	return [...]string{"", "orange", "yellow", "green", "blue", "purple", "gray"}[color]
}

// ParseFlagColor recognizes the stable name of a visible color.
func ParseFlagColor(key string) FlagColor {
	for _, color := range FlagColors() {
		if color.Key() == key {
			return color
		}
	}
	return 0
}

// FlagBitKeywords gives the case-insensitive keyword for each bit, low first.
func FlagBitKeywords() [3]string {
	return [3]string{"$MailFlagBit0", "$MailFlagBit1", "$MailFlagBit2"}
}

// FlagBits returns the observed keyword mask, including the unassigned 111.
func FlagBits(snapshot []string) int {
	var bits int
	for i, bit := range FlagBitKeywords() {
		for _, name := range snapshot {
			if strings.EqualFold(name, bit) {
				bits |= 1 << i
				break
			}
		}
	}
	return bits
}
