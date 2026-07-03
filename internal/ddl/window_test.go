package ddl

import "testing"

func TestWindowValidate(t *testing.T) {
	tests := []struct {
		name    string
		w       Window
		wantErr bool
	}{
		{"ok same-day", Window{Start: "01:00", End: "05:00"}, false},
		{"ok overnight", Window{Start: "22:00", End: "05:00"}, false},
		{"ok with days", Window{Start: "01:00", End: "05:00", Days: []string{"Sat", "sun"}}, false},
		{"bad start format", Window{Start: "1am", End: "05:00"}, true},
		{"bad hour", Window{Start: "24:00", End: "05:00"}, true},
		{"bad minute", Window{Start: "01:60", End: "05:00"}, true},
		{"equal start/end", Window{Start: "01:00", End: "01:00"}, true},
		{"unknown day", Window{Start: "01:00", End: "05:00", Days: []string{"Funday"}}, true},
		{"empty start", Window{Start: "", End: "05:00"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
