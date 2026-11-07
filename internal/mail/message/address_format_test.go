package message

import "testing"

// A reply to a sender whose name holds a comma, an "@" or a colon used to
// fail with "Please enter at least one recipient." (#94): the name went into
// the To field unquoted.
func TestFormatAddressReadsBackAsTheSameAddress(t *testing.T) {
	for _, tc := range []struct {
		name, address, want string
	}{
		{"Ana Ruiz", "ana@example.com", "Ana Ruiz <ana@example.com>"},
		{"Ruiz, Ana", "ana@example.com", `"Ruiz, Ana" <ana@example.com>`},
		{"ana@example.com", "ana@example.com", "ana@example.com"},
		{"ANA@example.com", "ana@example.com", "ana@example.com"},
		{"ventas@example.com", "ana@example.com", `"ventas@example.com" <ana@example.com>`},
		{"Tienda: Avisos", "avisos@example.com", `"Tienda: Avisos" <avisos@example.com>`},
		{`Say "hi" \ there`, "hi@example.com", `"Say \"hi\" \\ there" <hi@example.com>`},
		{"José Núñez", "jose@example.com", "José Núñez <jose@example.com>"},
		{"", "ana@example.com", "ana@example.com"},
		{"Ana", "", ""},
	} {
		got := FormatAddress(tc.name, tc.address)
		if got != tc.want {
			t.Fatalf("FormatAddress(%q, %q) = %q, want %q", tc.name, tc.address, got, tc.want)
		}
		if got == "" {
			continue
		}
		addrs, err := ParseAddressList(got)
		if err != nil || len(addrs) != 1 || addrs[0].Address != tc.address {
			t.Fatalf("ParseAddressList(%q) = %v, %v", got, addrs, err)
		}
	}
}

func TestParseAddressListReadsUnquotedNames(t *testing.T) {
	addrs, err := ParseAddressList("Tienda: Avisos <avisos@example.com>, ventas@example.com <ventas@example.com>, Ana [Ventas] <ana@example.com>")
	if err != nil {
		t.Fatalf("ParseAddressList: %v", err)
	}
	want := []struct{ name, address string }{
		{"Tienda: Avisos", "avisos@example.com"},
		{"ventas@example.com", "ventas@example.com"},
		{"Ana [Ventas]", "ana@example.com"},
	}
	if len(addrs) != len(want) {
		t.Fatalf("got %d addresses, want %d: %v", len(addrs), len(want), addrs)
	}
	for i, w := range want {
		if addrs[i].Name != w.name || addrs[i].Address != w.address {
			t.Fatalf("address %d = %q <%s>, want %q <%s>", i, addrs[i].Name, addrs[i].Address, w.name, w.address)
		}
	}

	for _, bad := range []string{"not an address", "Ana <not an address>", "ana@example.com, nobody"} {
		if _, err := ParseAddressList(bad); err == nil {
			t.Fatalf("ParseAddressList(%q) succeeded, want an error", bad)
		}
	}
}
