// SPDX-FileCopyrightText: 2024 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package bdplib

type Bdp interface {
	SyncDataTypes(stationType string, dataTypes []DataType) error
	// SyncStations sync stations in the bdp.
	//
	// If syncState is true, the state of the bdp will be synced with the slice of stations passed.
	//
	// onlyActivate is only considered if syncState is true.
	// If onlyActivate is true all active stations passed are inserted or activated (the fina state is: current + all active)
	// If onlyActive is false all passed stations are activated and all existing stations not in the list are deactivated.
	SyncStations(stationType string, stations []Station, syncState bool, onlyActivate bool) error
	PushData(stationType string, dataMap DataMap) error
	CreateDataMap() DataMap

	GetOrigin() string
}
