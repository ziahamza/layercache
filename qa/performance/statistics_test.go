package main

import "testing"

func TestSummarizeUsesMedianPairsAndKeepsNegativeSavings(t *testing.T) {
	tests := []struct {
		name       string
		cold, warm []float64
		coldMedian float64
		warmMedian float64
		ratio      float64
		passes     bool
	}{
		{"unordered odd samples", []float64{100, 300, 200}, []float64{60, 30, 50}, 200, 50, 0.25, true},
		{"even samples and boundary", []float64{400, 100, 300, 200}, []float64{150, 50, 200, 100}, 250, 125, 0.5, true},
		{"slower restoration", []float64{100, 200, 300}, []float64{600, 500, 400}, 200, 500, 2.5, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := summarize(test.cold, test.warm)
			if err != nil {
				t.Fatal(err)
			}
			if got.ColdMedianMS != test.coldMedian || got.WarmMedianMS != test.warmMedian || got.WarmColdRatio != test.ratio || got.Passed != test.passes {
				t.Fatalf("summary = %+v", got)
			}
		})
	}
}

func TestSummarizeRejectsMissingOrInvalidMeasurements(t *testing.T) {
	for _, pair := range []struct{ cold, warm []float64 }{
		{nil, nil}, {[]float64{100}, nil}, {[]float64{0}, []float64{1}}, {[]float64{1}, []float64{-1}},
	} {
		if _, err := summarize(pair.cold, pair.warm); err == nil {
			t.Fatal("invalid measurements were accepted")
		}
	}
}
