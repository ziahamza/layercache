package main

import (
	"math"
	"os/exec"
	"testing"
	"time"
)

func TestCalibrationProbeReportsActualConsumedCPU(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required for the real CPU probe")
	}
	got, err := calibrate(t.Context(), t.TempDir(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != "process.cpuUsage" || got.Probe.UserUS+got.Probe.SystemUS <= 0 || got.TargetCPUMS != 2000 || got.ProbeIterations != 250000 {
		t.Fatalf("CPU calibration evidence missing: %+v", got)
	}
}

func TestCalibrationUsesConsumedCPUDespiteElapsedTimeDrift(t *testing.T) {
	for _, wallMS := range []float64{50, 500, 5000} {
		got, err := calibrationFromProbe(cpuProbe{UserUS: 40000, SystemUS: 10000, WallMS: wallMS}, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// 250,000 rounds consumed 50ms of CPU: two CPU seconds requires
		// 10,000,000 rounds regardless of time spent waiting for a processor.
		if got.Iterations != 10000000 || got.Probe.UserUS != 40000 || got.Probe.SystemUS != 10000 || got.Probe.WallMS != wallMS {
			t.Fatalf("calibration = %+v", got)
		}
	}
}

func TestCalibrationRejectsMissingCPUAndUnboundedWork(t *testing.T) {
	for _, probe := range []cpuProbe{
		{WallMS: 50},
		{UserUS: -1, SystemUS: 10, WallMS: 50},
		{UserUS: 10, SystemUS: -1, WallMS: 50},
		{UserUS: 1, WallMS: 50},
		{UserUS: 1000, WallMS: math.Inf(1)},
	} {
		if _, err := calibrationFromProbe(probe, 2*time.Second); err == nil {
			t.Fatalf("invalid CPU probe accepted: %+v", probe)
		}
	}
}
