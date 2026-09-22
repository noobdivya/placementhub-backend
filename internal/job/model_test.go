package job

import (
	"strings"
	"testing"
	"time"

	"placementhub/internal/httpx"
)

func validInput() Input {
	return Input{Role: "Backend Engineer", Type: "Full-time", Location: "Pune", CTC: 12, MinCGPA: 7,
		Branches: []string{"CSE", "IT"}, Skills: []string{"Go"}, Deadline: "2026-10-10", Openings: 3,
		Description: "Build ingestion pipelines."}
}

func TestInputValidation(t *testing.T) {
	today := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	in := validInput()
	in.normalise()
	if err := in.validate(today); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	mut := map[string]func(*Input){
		"role":        func(i *Input) { i.Role = "x" },
		"type":        func(i *Input) { i.Type = "Gig" },
		"location":    func(i *Input) { i.Location = " " },
		"ctc":         func(i *Input) { i.CTC = -1 },
		"minCgpa":     func(i *Input) { i.MinCGPA = 10.5 },
		"openings":    func(i *Input) { i.Openings = 0 },
		"description": func(i *Input) { i.Description = "short" },
		"branches":    func(i *Input) { i.Branches = nil },
		"deadline":    func(i *Input) { i.Deadline = "10/10/2026" },
	}
	for field, f := range mut {
		bad := validInput()
		f(&bad)
		bad.normalise()
		err := bad.validate(today)
		he, ok := err.(*httpx.Error)
		if !ok || he.Fields[field] == "" {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	// Today is allowed, yesterday is not.
	in.Deadline = "2026-09-21"
	if err := in.validate(today); err != nil {
		t.Errorf("a deadline of today was rejected: %v", err)
	}
	in.Deadline = "2026-09-20"
	if err := in.validate(today); err == nil {
		t.Error("a deadline in the past was accepted")
	}
	tooMany := validInput()
	tooMany.Skills = make([]string, 21)
	for i := range tooMany.Skills {
		tooMany.Skills[i] = strings.Repeat("s", 3) + string(rune('a'+i))
	}
	tooMany.normalise()
	if tooMany.validate(today) == nil {
		t.Error("21 skills accepted")
	}
}

func TestNormaliseDedupesAndOrdersBranches(t *testing.T) {
	in := validInput()
	in.Branches = []string{"IT", " CSE ", "IT", "ECE"}
	in.Skills = []string{"React", "react", "  Node   JS ", ""}
	in.normalise()
	if strings.Join(in.Branches, ",") != "CSE,IT,ECE" {
		t.Errorf("branches = %v (want de-duplicated, in the standard order)", in.Branches)
	}
	if strings.Join(in.Skills, "|") != "React|Node JS" {
		t.Errorf("skills = %v", in.Skills)
	}
}

func TestDeadlineEndIsTheEndOfTheDeadlineDay(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	end, err := DeadlineEnd("2026-10-05", ist)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 6, 0, 0, 0, 0, ist); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
	// 23:59 IST on the deadline day is still in time; 00:00 the next day is not.
	if !time.Date(2026, 10, 5, 23, 59, 0, 0, ist).Before(end) || time.Date(2026, 10, 6, 0, 0, 0, 0, ist).Before(end) {
		t.Error("boundary wrong")
	}
	if _, err := DeadlineEnd("garbage", ist); err == nil {
		t.Error("garbage date accepted")
	}
	// Today() reads the calendar date in the college timezone, not UTC.
	late := time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC) // 01:30 on the 22nd in IST
	if got := Today(late, ist); got.Day() != 22 {
		t.Errorf("Today = %v, want the 22nd", got)
	}
}
