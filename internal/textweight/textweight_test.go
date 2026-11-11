package textweight

import "testing"

func TestParse(t *testing.T) {
	for value, want := range map[string]int{
		"":       0,
		"0":      0,
		"100":    100,
		" -200 ": -200,
		"150":    0,
		"300":    0,
		"bold":   0,
	} {
		if got := Parse(value); got != want {
			t.Errorf("Parse(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestValidAndNormalize(t *testing.T) {
	for value, want := range map[string]string{"": "", "0": "", "-100": "-100", " 200": "200"} {
		if !Valid(value) {
			t.Errorf("Valid(%q) = false", value)
		}
		if got := Normalize(value); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", value, got, want)
		}
	}
	for _, value := range []string{"50", "300", "-300", "bold", "100;}body{color:red"} {
		if Valid(value) {
			t.Errorf("Valid(%q) = true", value)
		}
	}
}

func TestCSS(t *testing.T) {
	settings := map[string]string{"weight_navigation": "100", "weight_lists": "-200"}
	get := func(key string) string { return settings[key] }
	if got, want := CSS(get), ":root{--weight-navigation:100;--weight-lists:-200;}"; got != want {
		t.Errorf("CSS = %q, want %q", got, want)
	}
	settings = map[string]string{"weight_lists": "x}"}
	if got := CSS(get); got != "" {
		t.Errorf("CSS with defaults = %q, want empty", got)
	}
}
