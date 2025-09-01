// SPDX-FileCopyrightText: 2025 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package elab

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/noi-techpark/go-bdp-client/bdplib"
	"github.com/noi-techpark/go-timeseries-client/odhts"
	"github.com/noi-techpark/go-timeseries-client/where"
)

type Elaboration struct {
	StationTypes            []string
	Filter                  string
	IncludeInactiveStations bool
	BaseTypes               []BaseDataType
	ElaboratedTypes         []ElaboratedDataType
	StartingPoint           time.Time
	b                       *bdplib.Bdp
	c                       *odhts.C
}

// arbitraty starting point where there should be no data yet
var minTime = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)

func NewElaboration(ts *odhts.C, bdp *bdplib.Bdp) Elaboration {
	return Elaboration{b: bdp, c: ts, StartingPoint: minTime, IncludeInactiveStations: false}
}

type BaseDataType struct {
	Name   string
	Period Period
}

type ElaboratedDataType struct {
	Name        string
	Period      Period
	Unit        string
	Description string
	Rtype       string
	MetaData    map[string]any
	DontSync    bool
}
type ElabResult struct {
	Timestamp   time.Time
	Period      Period
	StationType string
	StationCode string
	DataType    string
	Value       any
}

type DtoMeasurement struct {
	Period uint64       `json:"mperiod"`
	Time   odhts.TsTime `json:"mvalidtime"`
	Since  odhts.TsTime `json:"mtransactiontime"`
}

type DtoTreeData = map[string]struct {
	Stations map[string]struct {
		Station
		Datatypes map[string]struct {
			Measurements []DtoMeasurement `json:"tmeasurements"`
		} `json:"sdatatypes"`
	} `json:"stations"`
}
type Station struct {
	Stationcode string `json:"scode"`
	Name        string `json:"sname"`
	Origin      string `json:"sorigin"`
	Stationtype string `json:"stype"`
	Coord       struct {
		X    float32
		Y    float32
		Srid uint32
	} `json:"scoordinate"`
	Metadata map[string]any `json:"smetadata"`
}

type MeasurementValueType int

const (
	MTypeString MeasurementValueType = iota
	MTypeFloat
	MTypeObject
)

type MeasurementValue struct {
	Type MeasurementValueType
	Str  *string
	Num  *float64
	Obj  map[string]any
}

func (f *MeasurementValue) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		f.Str = &s
		f.Type = MTypeString
		return nil
	}

	var num float64
	if err := json.Unmarshal(data, &num); err == nil {
		f.Num = &num
		f.Type = MTypeFloat
		return nil
	}

	// Try map
	var m map[string]any
	if err := json.Unmarshal(data, &m); err == nil {
		f.Obj = m
		f.Type = MTypeObject
		return nil
	}

	return fmt.Errorf("unknown type for FlexibleField: %s", string(data))
}

type Measurement struct {
	Period      int              `json:"mperiod"`
	Timestamp   odhts.TsTime     `json:"mvalidtime"`
	TypeName    string           `json:"tname"`
	Value       MeasurementValue `json:"mvalue"`
	StationCode string           `json:"scode"`
	StationType string           `json:"stype"`
}
type Period = uint64

func (e Elaboration) SyncDataTypes() error {
	b := (*e.b)
	dts := map[string]bdplib.DataType{}
	for _, t := range e.ElaboratedTypes {
		if !t.DontSync {
			dts[t.Name] = bdplib.DataType{
				Name:        t.Name,
				Description: t.Description,
				Unit:        t.Unit,
				Rtype:       t.Rtype,
				MetaData:    t.MetaData,
			}
		}
	}
	return b.SyncDataTypes(slices.Collect(maps.Values(dts)))
}

func (e Elaboration) RequestState() (ElaborationState, error) {
	req := e.buildStateRequest()

	var res odhts.Response[DtoTreeData]
	err := odhts.Latest(*e.c, req, &res)
	if err != nil {
		slog.Error("error getting latest records from ninja. aborting...", "err", err)
		return ElaborationState{}, err
	}

	return mapNinja2ElabTree(res), nil
}

func (e Elaboration) buildStateRequest() *odhts.Request {
	req := odhts.DefaultRequest()
	req.Repr = odhts.TreeNode
	req.StationTypes = e.StationTypes

	datatypes := map[string]struct{}{}
	periods := map[string]struct{}{}
	for _, t := range e.BaseTypes {
		datatypes[t.Name] = struct{}{}
		periods[strconv.FormatUint(t.Period, 10)] = struct{}{}
	}
	for _, t := range e.ElaboratedTypes {
		datatypes[t.Name] = struct{}{}
		periods[strconv.FormatUint(t.Period, 10)] = struct{}{}
	}

	req.DataTypes = slices.Collect(maps.Keys(datatypes))
	periodsStr := strings.Join(slices.Collect(maps.Keys(periods)), ",")

	filters := []string{}
	if !e.IncludeInactiveStations {
		filters = append(filters, where.Eq("sactive", "true"))
	}
	if periodsStr != "" {
		filters = append(filters, where.In("mperiod", periodsStr))
	}
	if e.Filter != "" {
		filters = append(filters, e.Filter)
	}
	if len(filters) > 0 {
		req.Where = where.And(filters...)
	}

	req.Limit = -1
	return req
}

func mapNinja2ElabTree(o odhts.Response[DtoTreeData]) ElaborationState {
	// Tree is the same stationtype / stationcode / datatype structure
	e := ElaborationState{}
	for k, v := range o.Data {
		stype := ESStationType{}
		stype.Stations = map[string]ESStation{}
		for k, v := range v.Stations {
			st := ESStation{}
			st.Station = v.Station
			st.Datatypes = map[string]ESDataType{}
			for k, v := range v.Datatypes {
				dt := ESDataType{}
				dt.Periods = map[Period]time.Time{}
				for _, m := range v.Measurements {
					// since this is supposed to be a /latest request, periods are assumed to be unique
					dt.Periods[Period(m.Period)] = m.Time.Time
				}
				st.Datatypes[k] = dt
			}
			stype.Stations[k] = st
		}
		e[k] = stype
	}

	return e
}

type ESStationType struct {
	Stations map[string]ESStation
}

type ESStation struct {
	Station   Station
	Datatypes map[string]ESDataType
}

type ESDataType struct {
	Periods map[Period]time.Time
}

type ElaborationState = map[string]ESStationType

func (e Elaboration) RequestHistory(stationtypes []string, stationcodes []string, datatypes []string, periods []Period, from time.Time, to time.Time) ([]Measurement, error) {
	var ret []Measurement
	// limit currently fixed to -1, so page should always be only one
	limit := -1
	for page := 0; ; page += 1 {
		req := e.buildHistoryRequest(stationtypes, stationcodes, datatypes, periods, from, to)

		req.Select = "mvalue,mperiod,mvalidtime,scode,stype,tname"
		req.Limit = limit
		req.Offset = uint(page * req.Limit)

		res := &odhts.Response[[]Measurement]{}

		err := odhts.History(*e.c, req, res)
		if err != nil {
			return nil, err
		}

		ret = append(ret, res.Data...)

		// only if limit = length, there might be more data
		if res.Limit != int64(len(res.Data)) {
			break
		} else {
			slog.Debug("Using pagination to request more data: ", "limit", res.Limit, "data.length", len(res.Data), "offset", req.Offset, "firstDate", res.Data[0].Timestamp.Time)
		}
	}
	return ret, nil
}
func (e Elaboration) buildHistoryRequest(stationtypes []string, stationcodes []string, datatypes []string, periods []Period, from time.Time, to time.Time) *odhts.Request {
	req := odhts.DefaultRequest()
	req.Repr = odhts.FlatNode
	req.StationTypes = stationtypes
	req.From = from
	req.To = to

	req.DataTypes = datatypes

	periodsStr := []string{}
	for _, period := range periods {
		periodsStr = append(periodsStr, strconv.FormatUint(period, 10))
	}

	filters := []string{}
	if !e.IncludeInactiveStations {
		filters = append(filters, where.Eq("sactive", "true"))
	}
	if len(periodsStr) > 0 {
		filters = append(filters, where.In("mperiod", periodsStr...))
	}
	if len(stationcodes) > 0 {
		filters = append(filters, where.In("scode", where.EscapeList(stationcodes...)...))
	}
	if e.Filter != "" {
		filters = append(filters, e.Filter)
	}
	if len(filters) > 0 {
		req.Where = where.And(filters...)
	}

	req.Limit = -1
	return req
}

// we find the earliest elaborated type (e.g. the end of previous elaboration) and the latest base type
// TODO: make this a method of ESStation
func (e Elaboration) stationCatchupInterval(s ESStation) (from time.Time, to time.Time, estimatedMeasurements uint64) {
	for _, t := range e.ElaboratedTypes {
		edt := s.Datatypes[t.Name]
		if edt.Periods != nil {
			per := edt.Periods[t.Period]
			if from.IsZero() || from.After(per) {
				from = per
			}
		}
	}

	for _, t := range e.BaseTypes {
		edt := s.Datatypes[t.Name]
		if edt.Periods != nil {
			per := edt.Periods[t.Period]
			if to.Before(per) {
				// add a milliseconds because interval of ninja is half-open, and we actually want to get the last record
				to = per.Add(time.Millisecond)
			}
			// we assume that every period has a history for interval [from:per], and that records actually have that periodicity
			if !per.IsZero() && t.Period > 0 {
				intervalSeconds := per.Sub(from).Seconds()
				if intervalSeconds > 0 {
					estimatedMeasurements += uint64(intervalSeconds) / t.Period
				}
			}
		}
	}
	if from.IsZero() {
		from = e.StartingPoint
	}

	return
}

// Push elaboration results to timeseries writer
func (e Elaboration) PushResults(stationtype string, results []ElabResult) error {
	if len(results) == 0 {
		return nil
	}
	b := (*e.b)
	dm := b.CreateDataMap()
	for _, r := range results {
		dm.AddRecord(r.StationCode, r.DataType, bdplib.CreateRecord(r.Timestamp.UnixMilli(), r.Value, r.Period))
	}
	return b.PushData(stationtype, dm)
}
