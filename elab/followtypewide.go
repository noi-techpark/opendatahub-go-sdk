// SPDX-FileCopyrightText: 2025 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package elab

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// Track last timestamp per station, BaseDataType name, and period using sync.Map
var whideFollowerLastTimestamps sync.Map // map[string]time.Time where key is "stationcode:name:period"

type wideTypeFollower struct {
	ChunkDuration time.Duration
	MaxUrlLength  uint64
	e             Elaboration
}

func (e Elaboration) NewWideTypeFollower(timeChunk time.Duration) *wideTypeFollower {
	return &wideTypeFollower{
		ChunkDuration: timeChunk,
		MaxUrlLength:  1000, // Default URL length limit
		e:             e,
	}
}

// makeTimestampKey creates a unique key for storing timestamps by stationcode, BaseDataType name, and period
func (f *wideTypeFollower) makeTimestampKey(stationCode, baseTypeName string, period Period) string {
	return fmt.Sprintf("%s:%s:%d", stationCode, baseTypeName, period)
}

// getLastTimestamp returns the last processed timestamp for a station, BaseDataType name, and period
func (f *wideTypeFollower) getLastTimestamp(stationCode, baseTypeName string, period Period) (time.Time, bool) {
	key := f.makeTimestampKey(stationCode, baseTypeName, period)
	if value, exists := whideFollowerLastTimestamps.Load(key); exists {
		return value.(time.Time), true
	}
	return time.Time{}, false
}

// setLastTimestamp updates the last processed timestamp for a station, BaseDataType name, and period
func (f *wideTypeFollower) setLastTimestamp(stationCode, baseTypeName string, period Period, timestamp time.Time) {
	key := f.makeTimestampKey(stationCode, baseTypeName, period)
	whideFollowerLastTimestamps.Store(key, timestamp)
}

// updateLastTimestampFromMeasurements updates the last timestamp for the specific station based on their latest measurements
func (f *wideTypeFollower) updateLastTimestampFromMeasurements(measurements []ElabResult, stationCode string, baseType BaseDataType) {
	if len(measurements) == 0 {
		return
	}

	// Find the latest timestamp in the measurements for this station
	var latestTimestamp time.Time
	for _, m := range measurements {
		if m.Timestamp.After(latestTimestamp) {
			latestTimestamp = m.Timestamp
		}
	}

	// Update timestamp for the station
	f.setLastTimestamp(stationCode, baseType.Name, baseType.Period, latestTimestamp)
}

type stationCatchupInfo struct {
	stationType string
	station     Station
	from        time.Time
	to          time.Time
	estimated   uint64
}

// preComputeStationCatchupIntervals pre-computes catchup intervals for all stations for a specific base type
func (f *wideTypeFollower) preComputeStationCatchupIntervals(es ElaborationState, baseType BaseDataType) []stationCatchupInfo {
	var stationInfos []stationCatchupInfo

	for stationTypeName, stp := range es {
		for _, st := range stp.Stations {
			from, to, estimated := st.catchupIntervalForBaseType(f.e, &baseType)
			// Only include stations that have data for this base type
			if !to.After(from) {
				continue // Skip stations with no data to elaborate
			}
			stationInfos = append(stationInfos, stationCatchupInfo{
				stationType: stationTypeName,
				station:     st.Station,
				from:        from,
				to:          to,
				estimated:   estimated,
			})
		}
	}

	return stationInfos
}

// typeCatchupInterval finds the absolute overall time boundaries using pre-computed intervals (already filtered for base type)
func (f *wideTypeFollower) typeCatchupInterval(stationInfos []stationCatchupInfo) (from time.Time, to time.Time) {
	for _, info := range stationInfos {
		// Use the pre-computed from and to times (already filtered for the specific base type)
		if from.IsZero() || from.After(info.from) {
			from = info.from
		}

		if to.Before(info.to) {
			to = info.to
		}
	}

	if from.IsZero() {
		from = f.e.StartingPoint
	}

	return
}

// getStationsNeedingElaboration returns stations that need elaboration in the time chunk using pre-computed intervals and timestamp filtering
func (f *wideTypeFollower) getStationsNeedingElaboration(stationInfos []stationCatchupInfo, baseType BaseDataType, chunkStart, chunkEnd time.Time) (stationTypes []string, stationCodes []string) {
	stationTypeMap := map[string]struct{}{}
	stationCodeMap := map[string]struct{}{}

	for _, info := range stationInfos {
		// Station interval: [info.from, info.to]
		// Chunk interval: [chunkStart, chunkEnd]
		// Intersection exists if: info.from < chunkEnd AND chunkStart < info.to
		if info.from.Before(chunkEnd) && chunkStart.Before(info.to) {
			// Additional filtering: check if station has processed data beyond the chunk start time
			lastTimestamp, exists := f.getLastTimestamp(info.station.Stationcode, baseType.Name, baseType.Period)

			// Include station if:
			// 1. No previous timestamp exists (never processed), OR
			// 2. Last processed timestamp is before chunk end (might have new data in this chunk)
			if !exists || lastTimestamp.Before(chunkEnd) {
				stationTypeMap[info.stationType] = struct{}{}
				stationCodeMap[info.station.Stationcode] = struct{}{}
			}
		}
	}

	stationTypes = slices.Collect(maps.Keys(stationTypeMap))
	stationCodes = slices.Collect(maps.Keys(stationCodeMap))

	return
}

// chunkStationCodes splits station codes into chunks that don't exceed URL length limit
func (f *wideTypeFollower) chunkStationCodes(stationCodes []string, maxUrlLength uint64) [][]string {
	if len(stationCodes) == 0 {
		return nil
	}

	var chunks [][]string
	var currentChunk []string

	for _, code := range stationCodes {
		testChunk := append(currentChunk, code)
		if estimateStationFilterLength(testChunk) > maxUrlLength {
			// Current chunk would exceed limit, start new chunk
			if len(currentChunk) > 0 {
				chunks = append(chunks, currentChunk)
			}
			currentChunk = []string{code}
		} else {
			currentChunk = testChunk
		}
	}

	// Add the last chunk
	if len(currentChunk) > 0 {
		chunks = append(chunks, currentChunk)
	}

	return chunks
}

// Elaborate processes all measurements by base type using time chunking with pre-computed intervals
func (f *wideTypeFollower) Elaborate(es ElaborationState, handle func(t BaseDataType, from time.Time, to time.Time, s Station, ms []Measurement) ([]ElabResult, error)) {
	// Build station lookup map
	stations := map[string]Station{}
	for _, stp := range es {
		for _, st := range stp.Stations {
			stations[st.Station.Stationcode] = st.Station
		}
	}

	// Process each base type
	for _, baseType := range f.e.BaseTypes {
		// Pre-compute catchup intervals for all stations for this specific base type
		stationInfos := f.preComputeStationCatchupIntervals(es, baseType)

		if len(stationInfos) == 0 {
			slog.Debug("no stations need processing for base type", "type", baseType.Name)
			continue
		}

		from, to := f.typeCatchupInterval(stationInfos)

		if !to.After(from) {
			slog.Debug("no data to process for base type", "type", baseType.Name)
			continue
		}

		slog.Info("processing base type", "baseType", baseType.Name, "period", baseType.Period, "from", from, "to", to, "stationCount", len(stationInfos))

		// Process in chunks
		for chunkStart := from; chunkStart.Before(to); chunkStart = chunkStart.Add(f.ChunkDuration) {
			chunkEnd := chunkStart.Add(f.ChunkDuration)
			if chunkEnd.After(to) {
				chunkEnd = to
			}

			slog.Debug("processing chunk", "baseType", baseType.Name, "from", chunkStart, "to", chunkEnd)

			// Get stations that need elaboration for this chunk using pre-computed intervals and timestamp filtering
			stationTypes, stationCodes := f.getStationsNeedingElaboration(stationInfos, baseType, chunkStart, chunkEnd)

			if len(stationCodes) == 0 {
				slog.Debug("no stations need elaboration for chunk", "baseType", baseType.Name, "from", chunkStart, "to", chunkEnd)
				continue
			}

			// Chunk station codes to avoid URL limit
			stationChunks := f.chunkStationCodes(stationCodes, f.MaxUrlLength)

			// Process each station's measurements
			allResults := []ElabResult{}

			for _, stationChunk := range stationChunks {
				slog.Debug("processing station chunk", "baseType", baseType.Name, "stationCount", len(stationChunk), "from", chunkStart, "to", chunkEnd)

				// Get all measurements for this base type in this time chunk for these stations
				ms, err := f.e.RequestHistory(stationTypes, stationChunk, []string{baseType.Name}, []Period{baseType.Period}, chunkStart, chunkEnd)
				if err != nil {
					slog.Error("failed requesting data for base type", "baseType", baseType.Name, "from", chunkStart, "to", chunkEnd, "stationCount", len(stationChunk), "err", err)
					continue
				}

				// Group measurements by station first
				stationMeasurements := map[string][]Measurement{}
				for _, m := range ms {
					stationMeasurements[m.StationCode] = append(stationMeasurements[m.StationCode], m)
				}

				// Filter measurements per station (more efficient - only one sync.Map access per station)
				filteredStationMeasurements := map[string][]Measurement{}
				totalNewMeasurements := 0

				for stationCode, measurements := range stationMeasurements {
					// Get last timestamp for this station once
					lastTimestamp, exists := f.getLastTimestamp(stationCode, baseType.Name, baseType.Period)

					var newMeasurements []Measurement
					for _, m := range measurements {
						if !exists || m.Timestamp.Time.After(lastTimestamp) {
							newMeasurements = append(newMeasurements, m)
						}
					}

					if len(newMeasurements) > 0 {
						filteredStationMeasurements[stationCode] = newMeasurements
						totalNewMeasurements += len(newMeasurements)
					}
				}

				if totalNewMeasurements == 0 {
					slog.Debug("no new measurements for baseType", "baseType", baseType.Name)
					continue
				}

				slog.Info("processing batch", "baseType", baseType.Name, "from", chunkStart, "to", chunkEnd, "s_cnt", len(stationMeasurements), "ms_cnt", len(ms))

				for stationCode, measurements := range filteredStationMeasurements {
					station := stations[stationCode]
					stationResults, err := handle(baseType, chunkStart, chunkEnd, station, measurements)
					if err != nil {
						slog.Error("error during elaboration of station", "station", station, "baseType", baseType.Name, "err", err)
						// when erroring, lets' mark the station as processed untile chunkend to avoid log spam and traffic
						f.updateLastTimestampFromMeasurements([]ElabResult{{Timestamp: chunkEnd}}, stationCode, baseType)
						continue
					}
					// Update last timestamp after successful processing of all stations
					if len(stationResults) > 0 {
						f.updateLastTimestampFromMeasurements(stationResults, stationCode, baseType)
					}
					allResults = append(allResults, stationResults...)
				}
			}

			// Push results for all station types (we need to group by station type) after stationChunks
			resultsByType := map[string][]ElabResult{}
			for _, result := range allResults {
				resultsByType[result.StationType] = append(resultsByType[result.StationType], result)
			}

			for stationType, results := range resultsByType {
				if err := f.e.PushResults(stationType, results); err != nil {
					slog.Error("error pushing results", "stationType", stationType, "baseType", baseType.Name, "err", err)
				}
			}
		}
	}
}
