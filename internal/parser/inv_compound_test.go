package parser

import "testing"

func TestSuspiciousInvNumberFlagsCompoundAndRange(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ДСК0454;ДСК0454/1;ДСК0454/2;ДСК0454/3;ДСК0454/4", true},
		{"БП-000387/ДСК 0135", true},
		{"ДСК0091/ДСК0274", true},
		{"ДСК0263-0267", true},
		// Законные формы — предупреждать не о чем.
		{"ДСК 0001", false},
		{"ДСК0586/1", false},
		{"БП-000336", false},
		{"ДСК 012739", false},
	}
	for _, c := range cases {
		got := suspiciousInvNumber(c.in) != ""
		if got != c.want {
			t.Errorf("suspiciousInvNumber(%q) = %v, ожидалось %v", c.in, got, c.want)
		}
	}
}
