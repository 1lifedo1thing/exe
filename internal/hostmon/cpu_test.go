package hostmon

import (
	"slices"
	"testing"
)

func TestCleanModel(t *testing.T) {
	for in, want := range map[string]string{
		"Intel(R) Xeon(R) w9-3475X":                        "Intel Xeon w9-3475X",
		"AMD Ryzen 9 5900X 12-Core Processor             ": "AMD Ryzen 9 5900X",
		"Intel(R) Core(TM) i7-8700K CPU @ 3.70GHz":         "Intel Core i7-8700K",
		"AMD EPYC 7763 64-Core Processor":                  "AMD EPYC 7763",
	} {
		if got := cleanModel(in); got != want {
			t.Errorf("cleanModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassKinds(t *testing.T) {
	kinds, kind := classKinds([]int{0, 0, 0}, "AMD Ryzen 9 5900X")
	if !slices.Equal(kinds, []string{"AMD Ryzen 9 5900X"}) || kind[0] != 0 {
		t.Errorf("one class: %v %v", kinds, kind)
	}
	// a hybrid Intel: the P-cores are the higher class
	kinds, kind = classKinds([]int{1, 1, 0, 0, 0}, "Intel Core i9-13900K")
	if !slices.Equal(kinds, []string{"Performance", "Efficiency"}) || kind[1] != 0 || kind[0] != 1 {
		t.Errorf("two classes: %v %v", kinds, kind)
	}
	kinds, kind = classKinds([]int{2, 1, 0}, "Intel Core Ultra 7 155H")
	if !slices.Equal(kinds, []string{"Performance", "Efficiency", "Low-power"}) || kind[0] != 2 {
		t.Errorf("three classes: %v %v", kinds, kind)
	}
}
