package calc

import "testing"

func TestAdd(t *testing.T) {
	cases := []struct{ a, b, want int }{{1, 2, 3}, {2, 2, 5}}
	for _, c := range cases {
		t.Run("case", func(t *testing.T) {
			if got := Add(c.a, c.b); got != c.want {
				t.Errorf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestGreet(t *testing.T) {
	got := Greet("rhun")
	want := "Hello, rhun!\nWelcome."
	if got != want {
		t.Errorf("Greet mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestSkip(t *testing.T) {
	t.Skip("not today")
}
