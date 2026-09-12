package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const calibrationProbeIterations = 250000

type cpuProbe struct {
	UserUS   int64   `json:"userCpuUs"`
	SystemUS int64   `json:"systemCpuUs"`
	WallMS   float64 `json:"wallMs"`
}

type calibration struct {
	Method          string   `json:"method"`
	ProbeIterations int      `json:"probeIterations"`
	Probe           cpuProbe `json:"probe"`
	TargetCPUMS     int64    `json:"targetCpuMs"`
	Iterations      int      `json:"iterations"`
}

func calibrate(ctx context.Context, root string, target time.Duration) (calibration, error) {
	// CPU consumption excludes scheduler delays on a shared host. Wall time is
	// retained as evidence, but cannot shrink the subsequent fixed workload.
	script := fmt.Sprintf(`const {pbkdf2Sync}=require("node:crypto");
const wall=performance.now(); const cpu=process.cpuUsage();
pbkdf2Sync("calibrate","layercache-performance",%d,32,"sha256");
const consumed=process.cpuUsage(cpu);
console.log(JSON.stringify({userCpuUs:consumed.user,systemCpuUs:consumed.system,wallMs:performance.now()-wall}));`, calibrationProbeIterations)
	output, _, err := execute(ctx, root, "node", "-e", script)
	if err != nil {
		return calibration{}, err
	}
	var probe cpuProbe
	if err := json.Unmarshal([]byte(output), &probe); err != nil {
		return calibration{}, fmt.Errorf("decode measured CPU calibration: %w", err)
	}
	return calibrationFromProbe(probe, target)
}

func calibrationFromProbe(probe cpuProbe, target time.Duration) (calibration, error) {
	if probe.UserUS < 0 || probe.SystemUS < 0 || probe.UserUS > math.MaxInt64-probe.SystemUS ||
		probe.UserUS+probe.SystemUS == 0 || probe.WallMS <= 0 || math.IsNaN(probe.WallMS) || math.IsInf(probe.WallMS, 0) || target <= 0 {
		return calibration{}, errors.New("CPU calibration requires positive measured CPU and finite elapsed time")
	}
	iterations := math.Ceil(float64(calibrationProbeIterations) * float64(target.Microseconds()) / float64(probe.UserUS+probe.SystemUS))
	if iterations > math.MaxInt32 {
		return calibration{}, fmt.Errorf("CPU calibration requires an unsupported iteration count: %.0f", iterations)
	}
	return calibration{Method: "process.cpuUsage", ProbeIterations: calibrationProbeIterations, Probe: probe,
		TargetCPUMS: target.Milliseconds(), Iterations: max(calibrationProbeIterations, int(iterations))}, nil
}
