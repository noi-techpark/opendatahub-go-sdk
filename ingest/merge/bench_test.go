// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package merge

import (
	"encoding/json"
	"testing"
)

// A station as parking-skidata builds it, and a record as an operator authors
// it — realistic sizes, so the numbers mean something.
type benchStation struct {
	Id            string         `json:"id"`
	Name          string         `json:"name"`
	StationType   string         `json:"stationType,omitempty"`
	Latitude      float64        `json:"latitude"`
	Longitude     float64        `json:"longitude"`
	Origin        string         `json:"origin"`
	ParentStation string         `json:"parentStation,omitempty"`
	MetaData      map[string]any `json:"metaData"`
}

func newBenchStation() *benchStation {
	return &benchStation{
		Id:            "urn:parking:skidata:f383171a-0ec4-5371-b4f6-8ba7b866a55d",
		Name:          "0607242_0",
		StationType:   "ParkingStation",
		Origin:        "skidata_dynamicdata",
		ParentStation: "urn:parking:skidata:012a51b4-6859-54dd-abdf-84ebe93b7e46",
		MetaData: map[string]any{
			"provider_id": "0607242_0",
			"facility_id": "0607242",
			"carpark_id":  0,
			"capacity":    245,
		},
	}
}

func benchRecord(b *testing.B) map[string]any {
	b.Helper()
	var m map[string]any
	err := json.Unmarshal([]byte(`{
	  "names": {"it":"Parcheggio NOI Techpark","de":"Parkplatz NOI Techpark","en":"Parking NOI Techpark"},
	  "standard_name": "Parcheggio NOI Techpark",
	  "municipality": "Bolzano - Bozen",
	  "gps": {"lat":46.4,"lon":11.34},
	  "netex": {"type":"urbanParking","layout":"multistorey","charging":true,
	            "reservation":"noReservations","surveillance":true,
	            "vehicletypes":"allPassengerVehicles","hazard_prohibited":true}
	}`), &m)
	if err != nil {
		b.Fatal(err)
	}
	return m
}

// Apply alone: what the rules cost once both sides are already decoded.
func BenchmarkApply(b *testing.B) {
	record := benchRecord(b)
	target := map[string]any{
		"name": "0607242_0", "latitude": 0.0, "longitude": 0.0,
		"metaData": map[string]any{"provider_id": "0607242_0", "capacity": 245},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := parkingToStation.Apply(target, record); err != nil {
			b.Fatal(err)
		}
	}
}

// Into + Apply: the whole cost a transformer actually pays per station, since
// the target is a typed struct and has to round-trip through JSON.
func BenchmarkIntoApply(b *testing.B) {
	record := benchRecord(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newBenchStation()
		err := Into(s, func(d map[string]any) error {
			return parkingToStation.Apply(d, record)
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The round trip on its own, to show how much of the cost is the rules and how
// much is encoding/json.
func BenchmarkIntoNoRules(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newBenchStation()
		if err := Into(s, func(d map[string]any) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

// The hand-written equivalent this replaces: assign fields directly on the
// typed struct, no JSON anywhere. The floor to compare against.
func BenchmarkHandWrittenEquivalent(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := newBenchStation()
		s.Name = "Parcheggio NOI Techpark"
		s.Latitude = 46.4
		s.Longitude = 11.34
		s.MetaData["name_it"] = "Parcheggio NOI Techpark"
		s.MetaData["name_de"] = "Parkplatz NOI Techpark"
		s.MetaData["name_en"] = "Parking NOI Techpark"
		s.MetaData["standard_name"] = "Parcheggio NOI Techpark"
		s.MetaData["municipality"] = "Bolzano - Bozen"
		s.MetaData["netex_parking"] = map[string]any{
			"type": "urbanParking", "layout": "multistorey", "charging": true,
			"reservation": "noReservations", "surveillance": true,
			"vehicletypes": "allPassengerVehicles", "hazard_prohibited": true,
		}
	}
}
