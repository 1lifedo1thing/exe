package hostmon

import (
	"regexp"
	"sort"
	"strings"
)

// cleanModel drops the marks, the rated clock and the core count from an
// x86 model name: "Intel(R) Xeon(R) w9-3475X" → "Intel Xeon w9-3475X",
// "AMD Ryzen 9 5900X 12-Core Processor" → "AMD Ryzen 9 5900X" (the page
// counts the cores itself).
func cleanModel(s string) string {
	for _, m := range []string{"(R)", "(r)", "(TM)", "(tm)", " CPU", " Processor"} {
		s = strings.ReplaceAll(s, m, "")
	}
	if i := strings.Index(s, " @ "); i > 0 {
		s = s[:i]
	}
	s = coreCount.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.Join(strings.Fields(s), " ")
}

var coreCount = regexp.MustCompile(`\s+\d+-Core$`)

// classKinds names a hybrid processor's kinds of core from Windows'
// efficiency classes, higher the faster: one class is the model itself;
// with two, Performance and Efficiency, the faster first; a third, the
// slowest, Low-power. kind is each class's index in kinds.
func classKinds(classes []int, model string) (kinds []string, kind map[int]int) {
	set := map[int]bool{}
	for _, c := range classes {
		set[c] = true
	}
	order := make([]int, 0, len(set))
	for c := range set {
		order = append(order, c)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(order)))
	kind = map[int]int{}
	if len(order) <= 1 {
		for _, c := range order {
			kind[c] = 0
		}
		return []string{model}, kind
	}
	for i, c := range order {
		name := "Efficiency"
		switch {
		case i == 0:
			name = "Performance"
		case i == len(order)-1 && len(order) > 2:
			name = "Low-power"
		}
		kinds = append(kinds, name)
		kind[c] = i
	}
	return kinds, kind
}

// topology is each logical CPU (its kind and core) and the kinds' names.
type topology struct {
	cpus  []CPU
	kinds []string
}
