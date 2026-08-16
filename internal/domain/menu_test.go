package domain_test

import (
	"testing"
	"time"

	"github.com/ubilolxon/qrmenu/internal/domain"
)

func minutes(h, m int) *int32 {
	v := int32(h*60 + m)
	return &v
}

// at builds a local time on a known date. 2026-08-17 is a Monday, so weekday
// arithmetic in the tests is easy to reason about.
func at(weekdayOffset, hour, min int) time.Time {
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC) // Monday
	return base.AddDate(0, 0, weekdayOffset).Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
}

func TestMenuAvailableAt(t *testing.T) {
	tests := []struct {
		name string
		menu domain.Menu
		when time.Time
		want bool
	}{
		{
			name: "inactive menu is never available",
			menu: domain.Menu{IsActive: false},
			when: at(0, 12, 0),
			want: false,
		},
		{
			name: "no window and no days means always",
			menu: domain.Menu{IsActive: true},
			when: at(3, 4, 30),
			want: true,
		},
		{
			name: "inside a normal window",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(8, 0), AvailableToMin: minutes(11, 0)},
			when: at(0, 9, 30),
			want: true,
		},
		{
			name: "before a normal window",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(8, 0), AvailableToMin: minutes(11, 0)},
			when: at(0, 7, 59),
			want: false,
		},
		{
			// The end is exclusive, so an 08:00-11:00 breakfast stops at 11:00
			// sharp and a lunch menu starting at 11:00 takes over cleanly.
			name: "the end of a window is exclusive",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(8, 0), AvailableToMin: minutes(11, 0)},
			when: at(0, 11, 0),
			want: false,
		},
		{
			name: "the start of a window is inclusive",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(8, 0), AvailableToMin: minutes(11, 0)},
			when: at(0, 8, 0),
			want: true,
		},
		{
			name: "day restriction excludes other days",
			menu: domain.Menu{IsActive: true, Days: []int16{6, 7}}, // weekend only
			when: at(0, 12, 0),                                     // Monday
			want: false,
		},
		{
			name: "day restriction allows a listed day",
			menu: domain.Menu{IsActive: true, Days: []int16{6, 7}},
			when: at(5, 12, 0), // Saturday
			want: true,
		},

		// A window that crosses midnight is the case naive implementations get
		// wrong: from > to, so a simple "between" test fails for every minute.
		{
			name: "late-night window, before midnight",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(22, 0), AvailableToMin: minutes(2, 0)},
			when: at(0, 23, 30),
			want: true,
		},
		{
			name: "late-night window, after midnight",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(22, 0), AvailableToMin: minutes(2, 0)},
			when: at(1, 1, 0), // Tuesday 01:00 belongs to Monday's window
			want: true,
		},
		{
			name: "late-night window, in the gap during the day",
			menu: domain.Menu{IsActive: true, AvailableFromMin: minutes(22, 0), AvailableToMin: minutes(2, 0)},
			when: at(0, 12, 0),
			want: false,
		},
		{
			// The hours after midnight belong to the day the window opened, so
			// a Friday-night menu is still serving at 01:00 on Saturday.
			name: "wrapping window attributes early hours to the opening day",
			menu: domain.Menu{
				IsActive:         true,
				AvailableFromMin: minutes(22, 0),
				AvailableToMin:   minutes(2, 0),
				Days:             []int16{5}, // Friday
			},
			when: at(5, 1, 0), // Saturday 01:00
			want: true,
		},
		{
			name: "wrapping window rejects early hours of an unlisted day",
			menu: domain.Menu{
				IsActive:         true,
				AvailableFromMin: minutes(22, 0),
				AvailableToMin:   minutes(2, 0),
				Days:             []int16{5}, // Friday
			},
			when: at(1, 1, 0), // Tuesday 01:00 follows Monday, not Friday
			want: false,
		},
		{
			name: "sunday maps to ISO day 7",
			menu: domain.Menu{IsActive: true, Days: []int16{7}},
			when: at(6, 12, 0), // Sunday
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.menu.AvailableAt(tc.when); got != tc.want {
				t.Errorf("AvailableAt(%s) = %v, want %v", tc.when.Format("Mon 15:04"), got, tc.want)
			}
		})
	}
}

func TestValidateAvailability(t *testing.T) {
	tests := []struct {
		name    string
		from    *int32
		to      *int32
		days    []int16
		wantErr bool
	}{
		{"both empty is fine", nil, nil, nil, false},
		{"a complete window is fine", minutes(8, 0), minutes(11, 0), nil, false},
		{"only a start is rejected", minutes(8, 0), nil, nil, true},
		{"only an end is rejected", nil, minutes(11, 0), nil, true},
		{"identical endpoints are ambiguous", minutes(8, 0), minutes(8, 0), nil, true},
		{"out of range start", func() *int32 { v := int32(1440); return &v }(), minutes(2, 0), nil, true},
		{"negative start", func() *int32 { v := int32(-1); return &v }(), minutes(2, 0), nil, true},
		{"valid days", nil, nil, []int16{1, 3, 5}, false},
		{"day zero is not ISO", nil, nil, []int16{0}, true},
		{"day eight does not exist", nil, nil, []int16{8}, true},
		{"duplicate days", nil, nil, []int16{1, 1}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var v domain.Validator
			domain.ValidateAvailability(&v, tc.from, tc.to, tc.days)
			if gotErr := v.Err() != nil; gotErr != tc.wantErr {
				t.Errorf("error = %v, want error: %v", v.Err(), tc.wantErr)
			}
		})
	}
}

func TestNormalizeTags(t *testing.T) {
	got := domain.NormalizeTags([]string{" Vegan ", "SPICY", "vegan", "", "  "})
	want := []string{"vegan", "spicy"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v (order must be preserved)", got, want)
		}
	}
}
