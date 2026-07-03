package ddl

import (
	"testing"
	"time"
)

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

func TestWindowContains(t *testing.T) {
	// A fixed Saturday: 2022-01-01 is a Saturday.
	at := func(weekdayOffset, hh, mm int) time.Time {
		base := time.Date(2022, 1, 1, hh, mm, 0, 0, time.UTC) // Sat
		return base.AddDate(0, 0, weekdayOffset)
	}
	sameDay := Window{Start: "01:00", End: "05:00"}
	overnight := Window{Start: "22:00", End: "05:00"}
	satNight := Window{Start: "22:00", End: "05:00", Days: []string{"Sat"}}

	tests := []struct {
		name string
		w    Window
		t    time.Time
		want bool
	}{
		{"same-day inside", sameDay, at(0, 3, 0), true},
		{"same-day at start (inclusive)", sameDay, at(0, 1, 0), true},
		{"same-day at end (exclusive)", sameDay, at(0, 5, 0), false},
		{"same-day before", sameDay, at(0, 0, 59), false},
		{"overnight evening", overnight, at(0, 23, 0), true},
		{"overnight past midnight", overnight, at(1, 2, 0), true}, // Sunday 02:00
		{"overnight at end (exclusive)", overnight, at(1, 5, 0), false},
		{"overnight dead zone", overnight, at(0, 12, 0), false},
		{"sat-night opens Sat evening", satNight, at(0, 23, 0), true},     // Sat 23:00
		{"sat-night tail Sun morning", satNight, at(1, 2, 0), true},       // Sun 02:00 (opened Sat)
		{"sat-night Sun evening excluded", satNight, at(1, 23, 0), false}, // opens Sun -> not allowed
		{"sat-night Mon morning excluded", satNight, at(2, 2, 0), false},  // opened Sun -> not allowed
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Contains(tc.t); got != tc.want {
				t.Errorf("Contains(%s) = %v, want %v", tc.t.Format("Mon 15:04"), got, tc.want)
			}
		})
	}
}
