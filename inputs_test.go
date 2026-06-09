package monty

import "testing"

func TestInputsOfStructTags(t *testing.T) {
	type request struct {
		UserID     int `monty:"user_id"`
		MaxRetries int
		Ignored    string `monty:"-"`
	}

	inputs := InputsOf(request{UserID: 7, MaxRetries: 3, Ignored: "x"})
	if got := inputs["user_id"].Int(); got != 7 {
		t.Fatalf("user_id = %d, want 7", got)
	}
	if got := inputs["max_retries"].Int(); got != 3 {
		t.Fatalf("max_retries = %d, want 3", got)
	}
	if _, ok := inputs["ignored"]; ok {
		t.Fatal("ignored field was included")
	}
}

func TestNormalizeInputsRejectsNonDict(t *testing.T) {
	_, err := normalizeInputs(123)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSnakeCase(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"UserID", "user_id"},
		{"HTTPTimeout", "http_timeout"},
		{"HTTPServer", "http_server"},
		{"APIKey2", "api_key2"},
		{"ParseJSON", "parse_json"},
		{"MaxRetries", "max_retries"},
		{"A", "a"},
		{"ID", "id"},
		{"already_snake", "already_snake"},
		{"lower", "lower"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := snakeCase(tc.in); got != tc.want {
			t.Errorf("snakeCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSnakeCaseDrivesInputNames(t *testing.T) {
	type request struct {
		HTTPTimeout int
		UserID      int
	}
	inputs := InputsOf(request{HTTPTimeout: 30, UserID: 7})
	if got := inputs["http_timeout"].Int(); got != 30 {
		t.Fatalf("http_timeout = %d, want 30 (keys: %v)", got, inputs)
	}
	if got := inputs["user_id"].Int(); got != 7 {
		t.Fatalf("user_id = %d, want 7 (keys: %v)", got, inputs)
	}
}

func TestInputsOfEReportsConversionErrors(t *testing.T) {
	// InputsOf swallows the error into an empty Inputs...
	if got := InputsOf(123); len(got) != 0 {
		t.Fatalf("InputsOf(123) = %v, want empty", got)
	}
	// ...while InputsOfE surfaces it.
	if _, err := InputsOfE(123); err == nil {
		t.Fatal("InputsOfE(123) = nil error, want conversion error")
	}

	type request struct {
		UserID int `monty:"user_id"`
	}
	inputs, err := InputsOfE(request{UserID: 7})
	if err != nil {
		t.Fatalf("InputsOfE(struct): %v", err)
	}
	if got := inputs["user_id"].Int(); got != 7 {
		t.Fatalf("user_id = %d, want 7", got)
	}
}
