package quantity

import "testing"

func TestQuantities(t *testing.T) {
	for _, c := range []struct {
		s    string
		cpu  bool
		want int64
	}{{"1.5", true, 1500}, {"500m", true, 500}, {"4Gi", false, 4294967296}, {"0.001k", false, 1}, {"1.001Ki", false, 0}, {"0.0001", true, 0}, {"0", true, 0}, {"1024", false, 0}, {"1e3", true, 0}} {
		got, e := Parse(c.s, c.cpu)
		if c.want == 0 {
			if e == nil {
				t.Errorf("accepted %s", c.s)
			}
		} else if e != nil || got != c.want {
			t.Errorf("%s: %d %v", c.s, got, e)
		}
	}
	for _, n := range []int64{1, 1024, 1000001, 1 << 30} {
		got, e := Parse(Bytes(n), false)
		if e != nil || got != n {
			t.Fatal(n, got, e)
		}
	}
}
