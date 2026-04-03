package metrics

import (
	"testing"
)

func TestLabelsSort(t *testing.T) {
	ls := Labels{
		{Name: "z", Value: "1"},
		{Name: "a", Value: "2"},
		{Name: "m", Value: "3"},
	}
	ls.Sort()
	if ls[0].Name != "a" || ls[1].Name != "m" || ls[2].Name != "z" {
		t.Fatalf("expected sorted order a,m,z, got %v", ls)
	}
}

func TestLabelsCanonical(t *testing.T) {
	ls := Labels{
		{Name: "type", Value: "service"},
		{Name: "id", Value: "svc_1"},
	}
	got := ls.Canonical()
	want := "id=svc_1,type=service"
	if got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
}

func TestLabelsHash(t *testing.T) {
	ls1 := Labels{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}
	ls2 := Labels{{Name: "b", Value: "2"}, {Name: "a", Value: "1"}}
	if ls1.Hash() != ls2.Hash() {
		t.Fatal("same labels in different order should produce same hash")
	}

	ls3 := Labels{{Name: "a", Value: "1"}, {Name: "b", Value: "3"}}
	if ls1.Hash() == ls3.Hash() {
		t.Fatal("different labels should produce different hash")
	}
}

func TestLabelsGet(t *testing.T) {
	ls := Labels{{Name: "id", Value: "svc_1"}, {Name: "type", Value: "service"}}
	if got := ls.Get("id"); got != "svc_1" {
		t.Fatalf("Get(id) = %q, want svc_1", got)
	}
	if got := ls.Get("missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
}

func TestLabelsFromMap(t *testing.T) {
	m := map[string]string{"b": "2", "a": "1"}
	ls := LabelsFromMap(m)
	if ls[0].Name != "a" || ls[1].Name != "b" {
		t.Fatalf("LabelsFromMap should return sorted labels, got %v", ls)
	}
}

func TestLabelsMap(t *testing.T) {
	ls := Labels{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}
	m := ls.Map()
	if m["a"] != "1" || m["b"] != "2" || len(m) != 2 {
		t.Fatalf("Map() = %v, want {a:1, b:2}", m)
	}
}

func TestDataPointSeriesID(t *testing.T) {
	dp1 := DataPoint{Name: "cpu", Labels: Labels{{Name: "id", Value: "1"}}}
	dp2 := DataPoint{Name: "cpu", Labels: Labels{{Name: "id", Value: "1"}}}
	if dp1.SeriesID() != dp2.SeriesID() {
		t.Fatal("same name+labels should produce same SeriesID")
	}

	dp3 := DataPoint{Name: "cpu", Labels: Labels{{Name: "id", Value: "2"}}}
	if dp1.SeriesID() == dp3.SeriesID() {
		t.Fatal("different labels should produce different SeriesID")
	}

	dp4 := DataPoint{Name: "mem", Labels: Labels{{Name: "id", Value: "1"}}}
	if dp1.SeriesID() == dp4.SeriesID() {
		t.Fatal("different names should produce different SeriesID")
	}
}

func TestDataPointSeriesKey(t *testing.T) {
	dp := DataPoint{Name: "cpu", Labels: Labels{{Name: "id", Value: "svc_1"}, {Name: "type", Value: "service"}}}
	got := dp.SeriesKey()
	want := "cpu{id=svc_1,type=service}"
	if got != want {
		t.Fatalf("SeriesKey() = %q, want %q", got, want)
	}

	dpNoLabels := DataPoint{Name: "cpu"}
	if dpNoLabels.SeriesKey() != "cpu" {
		t.Fatalf("SeriesKey() with no labels = %q, want %q", dpNoLabels.SeriesKey(), "cpu")
	}
}

func TestMetricTypeString(t *testing.T) {
	tests := []struct {
		t    MetricType
		want string
	}{
		{Gauge, "gauge"},
		{Counter, "counter"},
		{Histogram, "histogram"},
		{MetricType(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("MetricType(%d).String() = %q, want %q", tt.t, got, tt.want)
		}
	}
}

func TestParseMetricType(t *testing.T) {
	for _, s := range []string{"gauge", "Gauge", "GAUGE"} {
		mt, err := ParseMetricType(s)
		if err != nil || mt != Gauge {
			t.Errorf("ParseMetricType(%q) = %v, %v", s, mt, err)
		}
	}
	_, err := ParseMetricType("invalid")
	if err == nil {
		t.Fatal("expected error for invalid metric type")
	}
}
