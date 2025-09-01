// SPDX-FileCopyrightText: 2025 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package elab

import (
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

type elabBucket struct {
	stationtype string
	drops       uint64
	stations    []Station
	from        time.Time
	to          time.Time
}

func scaleEstimate(before time.Duration, after time.Duration, estimate uint64) uint64 {
	if estimate == 0 || before.Minutes() == 0 {
		return 0
	}
	return uint64(after.Minutes() / before.Minutes() * float64(estimate))
}

func (e elabBucket) consolidate(drops uint64, station Station, from time.Time, to time.Time) elabBucket {
	newBucket := elabBucket{
		stationtype: e.stationtype,
		stations:    append(e.stations, station),
	}

	if e.from.IsZero() || e.from.After(from) {
		newBucket.from = from
	} else {
		newBucket.from = e.from
	}
	if e.to.Before(to) {
		newBucket.to = to
	} else {
		newBucket.to = e.to
	}

	// estimates are made for a specific interval. If the bucket interval is different, the estimate has to be scaled accordingly
	oldDrops := scaleEstimate(e.to.Sub(e.from), newBucket.to.Sub(newBucket.from), e.drops)
	newDrops := scaleEstimate(to.Sub(from), newBucket.to.Sub(newBucket.from), drops)
	newBucket.drops = oldDrops + newDrops
	return newBucket
}

func (b elabBucket) flush(e Elaboration, handle func(s Station, ms []Measurement) ([]ElabResult, error)) {
	dts := []string{}
	periods := []Period{}
	for _, t := range e.BaseTypes {
		dts = append(dts, t.Name)
		periods = append(periods, t.Period)
	}
	stations := map[string]Station{}
	for _, st := range b.stations {
		stations[st.Stationcode] = st
	}

	// request history for all stations in bucket at the same time
	ms, err := e.RequestHistory([]string{b.stationtype}, slices.Collect(maps.Keys(stations)), dts, periods, b.from, b.to)
	if err != nil {
		slog.Error("failed requesting data. discarding bucket ...", "bucket", b, "err", err)
		return
	}

	// sort out which measurements belong to which station
	stMs := map[string][]Measurement{}
	for _, m := range ms {
		stMs[m.StationCode] = append(stMs[m.StationCode], m)
	}

	allResults := []ElabResult{}
	// call handler for each individual station
	for scode, ms := range stMs {
		station := stations[scode]
		stationResults, err := handle(station, ms)
		if err != nil {
			slog.Error("error during elaboration of station. continuing...", "station", station, "err", err)
			continue
		}
		allResults = append(allResults, stationResults...)
	}

	if err := e.PushResults(b.stationtype, allResults); err != nil {
		slog.Error("error pushing results", "bucket", b, "err", err)
		return
	}
}

type stationFollower struct {
	// Station elaborations are grouped while their estimated measurement count does not exceed this
	BucketMax  uint64
	WorkerPool int
	e          Elaboration
}

func (e Elaboration) NewStationFollower() stationFollower {
	return stationFollower{
		BucketMax:  20000,
		WorkerPool: 10,
		e:          e,
	}
}

/*
Elaborate one station at a time, catch up from earlierst elaborated measurement to latest base measurement.
*/
func (f stationFollower) Elaborate(es ElaborationState, handle func(s Station, ms []Measurement) ([]ElabResult, error)) {
	wg := sync.WaitGroup{}
	workers := make(chan struct{}, f.WorkerPool)
	for stationtype, stp := range es {
		bucket := elabBucket{stationtype: stationtype}
		for _, st := range stp.Stations {
			from, to, estimatedMeasurements := f.e.stationCatchupInterval(st)
			if !to.After(from) {
				// No data or already caught up
				slog.Debug("not computing anything for station", "station", st)
				continue
			}

			// if it would overflowed the bucket, go flush the old one first, then create a new bucket
			// this avoids bundling "small" stations with big ones
			if estimatedMeasurements+bucket.drops > f.BucketMax {
				wg.Add(1)
				workers <- struct{}{}
				go func() {
					defer func() { <-workers; wg.Done() }()
					bucket.flush(f.e, handle)
				}()
				bucket = elabBucket{stationtype: stationtype}
			}
			slog.Debug("Adding bucket to elaboration", "stationcode", st.Station.Stationcode, "from", from, "to", to)
			bucket = bucket.consolidate(estimatedMeasurements, st.Station, from, to)
		}

		wg.Add(1)
		workers <- struct{}{}
		go func() {
			defer func() { <-workers; wg.Done() }()
			bucket.flush(f.e, handle)
		}()
		bucket = elabBucket{stationtype: stationtype}
	}
	wg.Wait()
}
