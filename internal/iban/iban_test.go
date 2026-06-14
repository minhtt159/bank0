package iban

import "testing"

func TestValidateKnownIBANs(t *testing.T) {
	valid := []string{
		"GB82WEST12345698765432",
		"GB82 WEST 1234 5698 7654 32", // printed form normalizes
		"DE89370400440532013000",
		"FR1420041010050500013M02606",
		"NL91ABNA0417164300",
		"SE4550000000058398257466",
		"AE070331234567890123456",
		"SA0380000000608010167519",
		"NO9386011117947",
		"CH9300762011623852957",
	}
	for _, s := range valid {
		if err := Validate(s); err != nil {
			t.Errorf("Validate(%q) = %v, want valid", s, err)
		}
	}

	invalid := []string{
		"GB82WEST12345698765431", // flipped check digit
		"DE89370400440532013001",
		"XX0000",                  // unknown country / too short
		"DE8937040044053201300",   // wrong length for DE
		"GB82WEST1234569876543!",  // non-alphanumeric
		"1B82WEST12345698765432",  // bad structure (digit where letter expected)
		"",
	}
	for _, s := range invalid {
		if Validate(s) == nil {
			t.Errorf("Validate(%q) = nil, want invalid", s)
		}
	}
}

func TestComputeMatchesPublishedCheckDigits(t *testing.T) {
	// check digits the generator must reproduce for known BBANs
	cases := []struct{ cc, bban, want string }{
		{"DE", "370400440532013000", "DE89370400440532013000"},
		{"GB", "WEST12345698765432", "GB82WEST12345698765432"},
		{"NL", "ABNA0417164300", "NL91ABNA0417164300"},
		{"CH", "00762011623852957", "CH9300762011623852957"},
	}
	for _, c := range cases {
		got, err := Compute(c.cc, c.bban)
		if err != nil {
			t.Fatalf("Compute(%s,%s): %v", c.cc, c.bban, err)
		}
		if got != c.want {
			t.Errorf("Compute(%s,%s) = %s, want %s", c.cc, c.bban, got, c.want)
		}
	}
}

func TestGenerateRoundTrips(t *testing.T) {
	for _, cc := range []string{"SE", "DE", "GB", "FR", "NO", "NL", "ES", "MT", "RU"} {
		for i := 0; i < 50; i++ {
			s, err := Generate(cc)
			if err != nil {
				t.Fatalf("Generate(%s): %v", cc, err)
			}
			if len(s) != CountryLengths[cc] {
				t.Errorf("Generate(%s) length %d, want %d", cc, len(s), CountryLengths[cc])
			}
			if !IsValid(s) {
				t.Errorf("Generate(%s) = %s is not valid", cc, s)
			}
		}
	}
}

func TestGenerateUnknownCountry(t *testing.T) {
	if _, err := Generate("ZZ"); err == nil {
		t.Error("Generate(ZZ) should error on unknown country")
	}
}
