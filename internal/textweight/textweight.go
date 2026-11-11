// Package textweight moves the weight of a kind of text, such as the
// sidebar's or the lists', heavier or lighter than Gofer's default. Each kind
// is a role: a UI setting holds its step, the page head turns the steps into
// CSS variables (see CSS), and assets/css/input.css shifts every weight inside
// the role's containers by them.
package textweight

import (
	"strconv"
	"strings"
)

// Role is a kind of text with its own weight setting.
type Role struct {
	// Name is the role in the setting key and the CSS variable.
	Name string
}

// Setting is the UI setting that holds the role's step.
func (r Role) Setting() string { return "weight_" + r.Name }

// Variable is the CSS variable that carries the step to the page.
func (r Role) Variable() string { return "--weight-" + r.Name }

// Roles are the kinds of text whose weight can be changed, in the order
// Settings lists them.
var Roles = []Role{{Name: "navigation"}, {Name: "lists"}}

// Steps are the allowed shifts, lightest first. 0 is Gofer's default, saved
// as "".
var Steps = []int{-200, -100, 0, 100, 200}

// Parse is the step a saved value stands for. An empty or unknown value is
// the default.
func Parse(value string) int {
	step, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	for _, allowed := range Steps {
		if step == allowed {
			return step
		}
	}
	return 0
}

// Valid reports whether value is a step Settings can save.
func Valid(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	step, err := strconv.Atoi(value)
	return err == nil && Parse(value) == step
}

// Normalize is how a valid value is saved: "" for the default, else the
// step as a plain number.
func Normalize(value string) string {
	step := Parse(value)
	if step == 0 {
		return ""
	}
	return strconv.Itoa(step)
}

// CSS sets the variables of the roles whose step isn't the default, or is ""
// when every role has the default. get reads a UI setting.
func CSS(get func(key string) string) string {
	var b strings.Builder
	for _, role := range Roles {
		step := Parse(get(role.Setting()))
		if step == 0 {
			continue
		}
		b.WriteString(role.Variable() + ":" + strconv.Itoa(step) + ";")
	}
	if b.Len() == 0 {
		return ""
	}
	return ":root{" + b.String() + "}"
}
