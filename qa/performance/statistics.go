package main

import (
	"errors"
	"math"
	"slices"
)

type summary struct {
	ColdMedianMS  float64 `json:"coldMedianMs"`
	WarmMedianMS  float64 `json:"warmMedianMs"`
	WarmColdRatio float64 `json:"warmColdRatio"`
	Passed        bool    `json:"passed"`
}

func summarize(cold, warm []float64) (summary, error) {
	if len(cold) == 0 || len(cold) != len(warm) {
		return summary{}, errors.New("cold and warm measurements must contain the same nonzero sample count")
	}
	for _, measurements := range [][]float64{cold, warm} {
		for _, value := range measurements {
			if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return summary{}, errors.New("durations must be positive finite milliseconds")
			}
		}
	}
	result := summary{ColdMedianMS: median(cold), WarmMedianMS: median(warm)}
	result.WarmColdRatio = result.WarmMedianMS / result.ColdMedianMS
	result.Passed = result.WarmColdRatio <= 0.5
	return result, nil
}

func median(values []float64) float64 {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}
