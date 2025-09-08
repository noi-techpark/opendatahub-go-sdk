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

type loggableElabBucket struct {
	StationType string
	Drops       uint64
	Stations    int
	From        time.Time
	To          time.Time
}

func (b elabBucket) toLoggable() loggableElabBucket {
	return loggableElabBucket{
		StationType: b.stationtype,
		Drops:       b.drops,
		Stations:    len(b.stations),
		From:        b.from,
		To:          b.to,
	}
}

func scaleEstimate(before time.Duration, after time.Duration, estimate uint64) uint64 {
	if estimate == 0 || before.Minutes() == 0 {
		return 0
	}
	return uint64(after.Minutes() / before.Minutes() * float64(estimate))
}

// estimateStationFilterLength calculates the length of the encoded query parameter for a comma-separated and escaped list of station codes.
// It accounts for the length of each station code, the surrounding double quotes, and the commas separating them.
func (b elabBucket) estimateStationFilterLength() uint64 {
	stations := map[string]Station{}
	for _, st := range b.stations {
		stations[st.Stationcode] = st
	}

	stationCodes := slices.Collect(maps.Keys(stations))

	if len(stationCodes) == 0 {
		return 0
	}

	// Calculate the total length of the station codes.
	var totalLength uint64
	for _, code := range stationCodes {
		totalLength += uint64(len(code))
	}

	// Each station code is enclosed in double quotes. The length of one double quote character is 1.
	// The total length of the quotes is 2 * the number of station codes.
	quoteLength := uint64(len(stationCodes) * 2)

	// The commas separating the station codes are escaped as "%2C" which has a length of 3 characters.
	// The total length from escaped commas is (N - 1) * 3.
	commaLength := uint64(len(stationCodes)-1) * 3

	// The total length is the sum of the length of the station codes, the quotes, and the escaped commas.
	return totalLength + quoteLength + commaLength
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
		slog.Error("failed requesting data. discarding bucket ...", "bucket", b.toLoggable(), "err", err)
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
		slog.Error("error pushing results", "bucket", b.toLoggable(), "err", err)
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
			if estimatedMeasurements+bucket.drops > f.BucketMax || bucket.estimateStationFilterLength() > 1000 {
				wg.Add(1)
				workers <- struct{}{}
				go func(b elabBucket) {
					defer func() { <-workers; wg.Done() }()
					b.flush(f.e, handle)
				}(bucket)
				bucket = elabBucket{stationtype: stationtype}
			}
			slog.Debug("Adding bucket to elaboration", "stationcode", st.Station.Stationcode, "from", from, "to", to)
			bucket = bucket.consolidate(estimatedMeasurements, st.Station, from, to)
		}

		wg.Add(1)
		workers <- struct{}{}
		go func(b elabBucket) {
			defer func() { <-workers; wg.Done() }()
			b.flush(f.e, handle)
		}(bucket)
		bucket = elabBucket{stationtype: stationtype}
	}
	wg.Wait()
}
