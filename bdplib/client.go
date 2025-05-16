// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package bdplib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
)

type Provenance struct {
	Lineage              string `json:"lineage"`
	DataCollector        string `json:"dataCollector"`
	DataCollectorVersion string `json:"dataCollectorVersion"`
}

type Station struct {
	Id            string                 `json:"id"`
	Name          string                 `json:"name"`
	StationType   string                 `json:"stationType,omitempty"`
	Latitude      float64                `json:"latitude"`
	Longitude     float64                `json:"longitude"`
	Origin        string                 `json:"origin"`
	ParentStation string                 `json:"parentStation,omitempty"`
	MetaData      map[string]interface{} `json:"metaData"`
}

type DataMap struct {
	Name       string             `json:"name"`
	Data       []Record           `json:"data"`
	Branch     map[string]DataMap `json:"branch"`
	Provenance string             `json:"provenance"`
}

type Record struct {
	Value     interface{} `json:"value"`
	Period    uint64      `json:"period"`
	Timestamp int64       `json:"timestamp"`
}

// maps https://github.com/noi-techpark/bdp-core/blob/main/dto/src/main/java/com/opendatahub/timeseries/bdp/dto/dto/EventDto.java
type Event struct {
	Uuid                string `json:"uuid"`
	Origin              string `json:"origin"`
	Category            string `json:"category"`
	EventSeriesUuid     string `json:"eventSeriesUuid"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	LocationDescription string `json:"locationDescription"`
	WktGeometry         string `json:"wktGeometry"`
	// Start time (included), Unix timestamp ms
	EventStart int64 `json:"eventStart"`
	// End time (excluded), Unix timestamp ms
	EventEnd   *int64                 `json:"eventEnd"`
	Provenance string                 `json:"provenance"`
	MetaData   map[string]interface{} `json:"metaData"`
}

const syncDataTypesPath string = "/syncDataTypes"
const syncStationsPath string = "/syncStations"
const pushRecordsPath string = "/pushRecords"
const getDateOfLastRecordPath string = "/getDateOfLastRecord"
const stationsPath string = "/stations"
const provenancePath string = "/provenance"
const eventsPath string = "/event"

type BdpImpl struct {
	ProvenanceUuid string
	BaseUrl        string
	Prv            string
	Prn            string
	Origin         string
	Auth           *Auth
}

func FromEnv() Bdp {
	b := BdpImpl{}
	b.BaseUrl = os.Getenv("BDP_BASE_URL") + "/json"
	b.Prv = os.Getenv("BDP_PROVENANCE_VERSION")
	b.Prn = os.Getenv("BDP_PROVENANCE_NAME")
	b.Origin = os.Getenv("BDP_ORIGIN")
	b.Auth = AuthFromEnv()
	return &b
}

func (b *BdpImpl) SyncDataTypes(stationType string, dataTypes []DataType) error {
	b.pushProvenance()

	slog.Debug("Syncing data types...")

	url := b.BaseUrl + syncDataTypesPath + "?stationType=" + stationType + "&prn=" + b.Prn + "&prv=" + b.Prv

	_, err := b.postToWriter(dataTypes, url)

	slog.Debug("Syncing data types done.")
	return err
}

func (b *BdpImpl) SyncStations(stationType string, stations []Station, syncState bool, onlyActivate bool) error {
	b.pushProvenance()

	slog.Info("Syncing " + strconv.Itoa(len(stations)) + " " + stationType + " stations...")
	url := b.BaseUrl + syncStationsPath + "/" + stationType + "?prn=" + b.Prn + "&prv=" + b.Prv + "&syncState=" + strconv.FormatBool(syncState) + "&onlyActivation=" + strconv.FormatBool(onlyActivate)
	_, err := b.postToWriter(stations, url)
	slog.Info("Syncing stations done.")
	return err
}

func (b *BdpImpl) PushData(stationType string, dataMap DataMap) error {
	b.pushProvenance()
	if dataMap.Provenance == "" {
		dataMap.Provenance = b.ProvenanceUuid
	}

	slog.Info("Pushing records...")
	url := b.BaseUrl + pushRecordsPath + "/" + stationType + "?prn=" + b.Prn + "&prv=" + b.Prv
	_, err := b.postToWriter(dataMap, url)
	slog.Info("Pushing records done.")
	return err
}

func CreateDataType(name string, unit string, description string, rtype string) DataType {
	// TODO add some checks
	return DataType{
		Name:        name,
		Unit:        unit,
		Description: description,
		Rtype:       rtype,
	}
}

func CreateStation(id string, name string, stationType string, lat float64, lon float64, origin string) Station {
	// TODO add some checks
	var station = Station{
		Name:        name,
		StationType: stationType,
		Latitude:    lat,
		Longitude:   lon,
		Origin:      origin,
		Id:          id,
	}
	return station
}
func CreateEvent(uuid string, name string, category string, eventSeriesUuid string, origin string) Event {
	// TODO add some checks
	var event = Event{
		Uuid:            uuid,
		Name:            name,
		Category:        category,
		EventSeriesUuid: eventSeriesUuid,
		Origin:          origin,
	}
	return event
}

func CreateRecord(ts int64, value interface{}, period uint64) Record {
	// TODO add some checks
	record := Record{
		Value:     value,
		Timestamp: ts,
		Period:    period,
	}
	return record
}

func (b *BdpImpl) GetOrigin() string {
	return b.Origin
}

func (b *BdpImpl) CreateDataMap() DataMap {
	var dataMap = DataMap{
		Name:       "(default)",
		Provenance: b.ProvenanceUuid,
		Branch:     make(map[string]DataMap),
	}
	return dataMap
}

// add an array of record to dataMap
func (dataMap *DataMap) AddRecords(stationCode string, dataType string, records []Record) {
	for _, record := range records {
		dataMap.AddRecord(stationCode, dataType, record)
	}
}

// add one single record to dataMap
func (dataMap *DataMap) AddRecord(stationCode string, dataType string, record Record) {

	if dataMap.Branch[stationCode].Name == "" {
		dataMap.Branch[stationCode] = DataMap{
			Name:   "(default)",
			Branch: make(map[string]DataMap),
		}
		slog.Debug("new station in branch " + stationCode)
	}

	if dataMap.Branch[stationCode].Branch[dataType].Name == "" {
		dataMap.Branch[stationCode].Branch[dataType] = DataMap{
			Name: "(default)",
			Data: []Record{record},
		}
		// to assign a value to a struct in a map, this code part is needed
		// https://stackoverflow.com/a/69006398/8794667
	} else if entry, ok := dataMap.Branch[stationCode].Branch[dataType]; ok {
		entry.Data = append(entry.Data, record)
		dataMap.Branch[stationCode].Branch[dataType] = entry
	}
}

func (b *BdpImpl) postToWriter(data interface{}, fullUrl string) (string, error) {
	json, err := json.Marshal(data)
	if err != nil {
		slog.Error("Error unmarshalling JSON POST data")
		return "", err
	}

	client := http.Client{}
	req, err := http.NewRequest("POST", fullUrl, bytes.NewBuffer(json))
	if err != nil {
		slog.Error("Error creating http POST request")
		return "", err
	}

	req.Header = http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + b.Auth.getToken()},
	}

	res, err := client.Do(req)
	if err != nil {
		slog.Error("Error performing POST")
		return "", err
	}

	resb, err := io.ReadAll(res.Body)
	if err != nil {
		slog.Error("Error reading from Response body")
		return "", err
	}
	ress := string(resb)

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		slog.Error("bdp POST returned with error", "statusCode", strconv.Itoa(res.StatusCode), "body", ress)
		return "", errors.New("bdp POST returned non-OK status: " + strconv.Itoa(res.StatusCode))
	}

	return ress, nil
}

func (b *BdpImpl) pushProvenance() {
	if len(b.ProvenanceUuid) > 0 {
		return
	}

	slog.Info("Pushing provenance...")
	slog.Info("prv: " + b.Prv + " prn: " + b.Prn)

	var provenance = Provenance{
		DataCollector:        b.Prn,
		DataCollectorVersion: b.Prv,
		Lineage:              b.Origin,
	}

	url := b.BaseUrl + provenancePath + "?&prn=" + b.Prn + "&prv=" + b.Prv

	res, err := b.postToWriter(provenance, url)

	if err != nil {
		slog.Error("error", "err", err)
	}

	b.ProvenanceUuid = res

	slog.Info("Pushing provenance done", "uuid", b.ProvenanceUuid)
}

func (b *BdpImpl) GetStations(stationType string, origin string) ([]Station, error) {
	slog.Debug("Fetching stations", "stationType", stationType, "origin", origin)

	url := b.BaseUrl + stationsPath + fmt.Sprintf("/%s/?origin=%s&prn=%s&prv=%s", stationType, origin, b.Prn, b.Prv)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("unable to create BDP HTTP Request: %w", err)
	}

	req.Header = http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + b.Auth.getToken()},
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error performing ninja request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, errors.New("ninja request returned non-OK status: " + strconv.Itoa(res.StatusCode))
	}

	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read response body: %w", err)
	}

	result := []Station{}
	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling response JSON to provided interface: %w", err)
	}

	return result, nil
}

func (b *BdpImpl) SyncEvents(events []Event) error {
	b.pushProvenance()

	slog.Info("Syncing " + strconv.Itoa(len(events)) + " events...")
	for i := range events {
		events[i].Provenance = b.ProvenanceUuid
	}
	url := b.BaseUrl + eventsPath + "?prn=" + b.Prn + "&prv=" + b.Prv
	_, err := b.postToWriter(events, url)
	slog.Info("Syncing events done.")
	return err
}
