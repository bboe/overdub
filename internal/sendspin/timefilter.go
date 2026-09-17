package sendspin

import (
	"math"
	"sync"
)

const (
	processVariance      = 0.0
	driftProcessVariance = 1e-11 * 1e-11
	forgetVarianceFactor = 2.0 * 2.0
	adaptiveCutoff       = 3.0
	adaptiveAfter        = 100
	maxErrorScale        = 0.5
	driftSignificance    = 2.0 * 2.0
)

type timeElement struct {
	lastUpdate int64
	offset     float64
	drift      float64
	useDrift   bool
}

type timeFilter struct {
	mu sync.Mutex

	lastUpdate int64
	count      int

	offset float64
	drift  float64

	offsetCovariance      float64
	offsetDriftCovariance float64
	driftCovariance       float64

	current timeElement
}

func newTimeFilter() *timeFilter {
	return &timeFilter{offsetCovariance: math.Inf(1)}
}

func (f *timeFilter) Update(measurement, maxError, taken int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if taken <= f.lastUpdate {
		return
	}
	dt := float64(taken - f.lastUpdate)
	f.lastUpdate = taken

	updateStdDev := float64(maxError) * maxErrorScale
	measurementVariance := updateStdDev * updateStdDev

	if f.count <= 0 {
		f.count++
		f.offset = float64(measurement)
		f.offsetCovariance = measurementVariance
		f.drift = 0
		f.current = timeElement{lastUpdate: f.lastUpdate, offset: f.offset}
		return
	}

	if f.count == 1 {
		f.count++
		f.drift = (float64(measurement) - f.offset) / dt
		f.offset = float64(measurement)
		f.driftCovariance = (f.offsetCovariance + measurementVariance) / (dt * dt)
		f.offsetCovariance = measurementVariance
		f.current = timeElement{lastUpdate: f.lastUpdate, offset: f.offset, drift: f.drift}
		return
	}

	offset := f.offset + f.drift*dt

	nextDriftCovariance := f.driftCovariance + dt*driftProcessVariance
	nextOffsetDriftCovariance := f.offsetDriftCovariance + f.driftCovariance*dt
	nextOffsetCovariance := f.offsetCovariance + 2*f.offsetDriftCovariance*dt +
		f.driftCovariance*dt*dt + dt*processVariance

	residual := float64(measurement) - offset
	if f.count < adaptiveAfter {
		f.count++
	} else if math.Abs(residual) > float64(maxError)*adaptiveCutoff {
		nextDriftCovariance *= forgetVarianceFactor
		nextOffsetDriftCovariance *= forgetVarianceFactor
		nextOffsetCovariance *= forgetVarianceFactor
	}

	uncertainty := 1.0 / math.Max(nextOffsetCovariance+measurementVariance, 1e-9)
	offsetGain := nextOffsetCovariance * uncertainty
	driftGain := nextOffsetDriftCovariance * uncertainty

	f.offset = offset + offsetGain*residual
	f.drift += driftGain * residual

	f.driftCovariance = nextDriftCovariance - driftGain*nextOffsetDriftCovariance
	f.offsetDriftCovariance = nextOffsetDriftCovariance - driftGain*nextOffsetCovariance
	f.offsetCovariance = nextOffsetCovariance - offsetGain*nextOffsetCovariance

	f.current = timeElement{
		lastUpdate: f.lastUpdate,
		offset:     f.offset,
		drift:      f.drift,
		useDrift:   f.drift*f.drift > driftSignificance*f.driftCovariance,
	}
}

func (f *timeFilter) element() timeElement {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

func (f *timeFilter) ServerTime(clientTime int64) int64 {
	e := f.element()
	drift := 0.0
	if e.useDrift {
		drift = e.drift
	}
	return clientTime + int64(math.Round(e.offset+drift*float64(clientTime-e.lastUpdate)))
}

func clientFrom(e timeElement, serverTime int64) int64 {
	drift := 0.0
	if e.useDrift {
		drift = e.drift
	}
	return int64(math.Round((float64(serverTime) - e.offset + drift*float64(e.lastUpdate)) /
		(1.0 + drift)))
}

func (f *timeFilter) ClientTime(serverTime int64) int64 {
	return clientFrom(f.element(), serverTime)
}

func (f *timeFilter) sample(serverTime int64) (client, spread, offset int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	converged, spread := f.stateLocked()
	if !converged {
		return 0, 0, 0, false
	}
	return clientFrom(f.current, serverTime), spread, int64(math.Round(f.current.offset)), true
}

func (f *timeFilter) Converged() bool {
	converged, _ := f.state()
	return converged
}

func (f *timeFilter) state() (bool, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateLocked()
}

func (f *timeFilter) stateLocked() (bool, int64) {
	if f.count < 2 || math.IsInf(f.offsetCovariance, 1) {
		return false, math.MaxInt64
	}
	return true, int64(math.Round(math.Sqrt(f.offsetCovariance)))
}
