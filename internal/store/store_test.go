package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

var toronto, _ = time.LoadLocation("America/Toronto")

func openT(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func sess(id, name, centre, cat string, start time.Time) schedule.Session {
	return schedule.Session{
		ID: id, Categories: []string{cat}, Name: name, Centre: centre,
		Start: start, End: start.Add(time.Hour),
		Address: schedule.Address{Street: "1 Main", City: "Markham", PostalCode: "L3R"},
	}
}

func d(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, toronto)
}

func ids(ss []schedule.Session) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func TestReplaceCategoryRoundTrip(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	a := sess("a", "Aqua", "Cornell Community Centre", "Aquafit", d(2, 9))
	a.Description = "desc"
	a.AllDay = true
	minA, spots := 18, 5
	a.MinAge, a.SpotsLeft = &minA, &spots
	a.Price, a.Spots, a.CourseID, a.BookingURL = "$5", "5 left", "c1", "http://x"
	a.ScrapedAt = d(1, 1)
	b := sess("b", "Swim", "Angus Glen", "Aquafit", d(3, 9))
	if err := s.ReplaceCategory(ctx, "Aquafit", []schedule.Session{a, b, a}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Sessions(ctx, schedule.Filter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	g := got[0]
	if !g.Start.Equal(a.Start) || !g.End.Equal(a.End) || !g.ScrapedAt.Equal(a.ScrapedAt) {
		t.Fatalf("times differ: %+v", g)
	}
	if g.Start.Location().String() != "America/Toronto" {
		t.Fatalf("loc %v", g.Start.Location())
	}
	if *g.MinAge != 18 || g.MaxAge != nil || *g.SpotsLeft != 5 || !g.AllDay || g.Price != "$5" ||
		g.Address != a.Address || g.Description != "desc" || g.CourseID != "c1" || g.BookingURL != "http://x" {
		t.Fatalf("fields differ: %+v", g)
	}
	if !reflect.DeepEqual(g.Categories, []string{"Aquafit"}) {
		t.Fatalf("cats %v", g.Categories)
	}
}

func TestReplaceCategoryRemovesMissing(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	a := sess("a", "A", "C", "X", d(2, 9))
	b := sess("b", "B", "C", "X", d(2, 10))
	s.ReplaceCategory(ctx, "X", []schedule.Session{a, b})
	s.ReplaceCategory(ctx, "X", []schedule.Session{a})
	got, _ := s.Sessions(ctx, schedule.Filter{})
	if !reflect.DeepEqual(ids(got), []string{"a"}) {
		t.Fatalf("%v", ids(got))
	}
}

func TestSessionInTwoCategories(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	a := sess("a", "A", "C", "Aquafit", d(2, 9))
	s.ReplaceCategory(ctx, "Aquafit", []schedule.Session{a})
	s.ReplaceCategory(ctx, "Swimming", []schedule.Session{a})
	got, _ := s.Sessions(ctx, schedule.Filter{})
	if len(got) != 1 || !reflect.DeepEqual(got[0].Categories, []string{"Aquafit", "Swimming"}) {
		t.Fatalf("%+v", got)
	}
	cc, err := s.Categories(ctx)
	want := []CategoryCount{{"Aquafit", 1}, {"Swimming", 1}}
	if err != nil || !reflect.DeepEqual(cc, want) {
		t.Fatalf("%v %v", cc, err)
	}
}

func TestReplaceCategoryKeepsSharedSession(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	a := sess("a", "A", "C", "Aquafit", d(2, 9))
	s.ReplaceCategory(ctx, "Aquafit", []schedule.Session{a})
	s.ReplaceCategory(ctx, "Swimming", []schedule.Session{a})
	if err := s.ReplaceCategory(ctx, "Swimming", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Sessions(ctx, schedule.Filter{})
	if len(got) != 1 || !reflect.DeepEqual(got[0].Categories, []string{"Aquafit"}) {
		t.Fatalf("%+v", got)
	}
}

func TestRemoveCategoriesExcept(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "X", []schedule.Session{sess("a", "A", "C", "X", d(2, 9))})
	s.ReplaceCategory(ctx, "Y", []schedule.Session{sess("b", "B", "C", "Y", d(2, 9))})
	if err := s.RemoveCategoriesExcept(ctx, []string{"X"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Sessions(ctx, schedule.Filter{})
	if !reflect.DeepEqual(ids(got), []string{"a"}) {
		t.Fatalf("%v", ids(got))
	}
	s.RemoveCategoriesExcept(ctx, nil)
	got, _ = s.Sessions(ctx, schedule.Filter{})
	if len(got) != 0 {
		t.Fatalf("%v", ids(got))
	}
}

func TestPrune(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "X", []schedule.Session{
		sess("old", "O", "C", "X", d(1, 9)),
		sess("new", "N", "C", "X", d(5, 9)),
	})
	n, err := s.Prune(ctx, d(3, 0))
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got, _ := s.Sessions(ctx, schedule.Filter{})
	if !reflect.DeepEqual(ids(got), []string{"new"}) {
		t.Fatalf("%v", ids(got))
	}
	cc, _ := s.Categories(ctx)
	if len(cc) != 1 || cc[0].Count != 1 {
		t.Fatalf("%v", cc)
	}
}

func TestFilterMatching(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "Aquafit", []schedule.Session{
		sess("a", "A", "Cornell Community Centre", "Aquafit", d(2, 9)),
		sess("b", "B", "Angus Glen Community Centre", "Aquafit", d(2, 10)),
	})
	s.ReplaceCategory(ctx, "Skating", []schedule.Session{
		sess("c", "C", "Cornell Community Centre", "Skating", d(2, 11)),
		sess("d", "D", "Thornhill", "Skating", d(2, 12)),
	})
	q := func(f schedule.Filter) []string {
		got, err := s.Sessions(ctx, f)
		if err != nil || got == nil {
			t.Fatalf("%v %v", got, err)
		}
		return ids(got)
	}
	if g := q(schedule.Filter{Centres: []string{"CORNELL"}}); !reflect.DeepEqual(g, []string{"a", "c"}) {
		t.Fatal(g)
	}
	if g := q(schedule.Filter{Centres: []string{"CORNELL", "angus glen"}}); !reflect.DeepEqual(g, []string{"a", "b", "c"}) {
		t.Fatal(g)
	}
	if g := q(schedule.Filter{Centres: []string{"CORNELL", "angus glen"}, Categories: []string{"aquafit"}}); !reflect.DeepEqual(g, []string{"a", "b"}) {
		t.Fatal(g)
	}
	if g := q(schedule.Filter{Categories: []string{"nope"}}); len(g) != 0 {
		t.Fatal(g)
	}
}

func TestFilterTimeRangeAndLimit(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "X", []schedule.Session{
		sess("a", "A", "C", "X", d(2, 9)),
		sess("b", "B", "C", "X", d(3, 9)),
		sess("c", "C", "C", "X", d(4, 9)),
	})
	got, _ := s.Sessions(ctx, schedule.Filter{From: d(3, 9), To: d(4, 9)})
	if !reflect.DeepEqual(ids(got), []string{"b"}) {
		t.Fatal(ids(got))
	}
	got, _ = s.Sessions(ctx, schedule.Filter{From: d(2, 10), Limit: 1})
	if !reflect.DeepEqual(ids(got), []string{"b"}) {
		t.Fatal(ids(got))
	}
}

func TestCentres(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "X", []schedule.Session{
		sess("a", "A", "Zed", "X", d(2, 9)),
		sess("b", "B", "Zed", "X", d(2, 10)),
		sess("c", "C", "Alpha", "X", d(2, 10)),
		sess("e", "E", "", "X", d(2, 10)),
	})
	cs, err := s.Centres(ctx)
	if err != nil || len(cs) != 2 || cs[0].Name != "Alpha" || cs[1].Count != 2 || cs[1].Address.City != "Markham" {
		t.Fatalf("%v %v", cs, err)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	s, _ := openT(t)
	ctx := context.Background()
	st, err := s.LoadStatus(ctx)
	if err != nil || !reflect.DeepEqual(st, schedule.Status{}) {
		t.Fatalf("%+v %v", st, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in := schedule.Status{LastAttempt: &now, LastSuccess: &now, Total: 3,
		Categories: []schedule.CategoryResult{{Name: "X", OK: true, Count: 3}}}
	if err := s.SaveStatus(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Total = 4
	if err := s.SaveStatus(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadStatus(ctx)
	if err != nil || out.Total != 4 || !out.LastSuccess.Equal(now) || len(out.Categories) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestReopenPersists(t *testing.T) {
	s, p := openT(t)
	ctx := context.Background()
	s.ReplaceCategory(ctx, "X", []schedule.Session{sess("a", "A", "C", "X", d(2, 9))})
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.Sessions(ctx, schedule.Filter{})
	if !reflect.DeepEqual(ids(got), []string{"a"}) {
		t.Fatal(ids(got))
	}
}
