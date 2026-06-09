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
