// SPDX-FileCopyrightText: 2025 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package elab

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/noi-techpark/go-timeseries-client/odhts"
	"gotest.tools/v3/assert"
)

func TestMapNinja2ElabTree(t *testing.T) {
	// Read the file contents
	byteValue, err := os.ReadFile("./testdata/echargingtree.json")
	assert.NilError(t, err, "cant read file")

	var o odhts.Response[DtoTreeData]
	err = json.Unmarshal(byteValue, &o)
	assert.NilError(t, err, "cant unmarshal json")
	et := mapNinja2ElabTree(o)

	bt, err := json.Marshal(et)
	assert.NilError(t, err, "cant marshal json")
	fmt.Printf("%s", string(bt))
}

func TestElaboration_buildInitialStateRequest(t *testing.T) {
	e := Elaboration{
		Filter:             "scode.eq.\"123\"",
		OnlyActiveStations: true,
		BaseTypes: []BaseDataType{
			{Name: "base1", Period: 60},
			{Name: "base2", Period: 60},
			{Name: "both", Period: 600},
			{Name: "both", Period: 3600},
		},
		ElaboratedTypes: []ElaboratedDataType{
			{Name: "der1", Period: 3600},
			{Name: "both", Period: 3600},
			{Name: "both", Period: 600},
		},
	}
	req := e.buildStateRequest()
	assert.Equal(t, req.Where, "and(sactive.eq.true,mperiod.in.(60,600,3600),scode.eq.\"123\")")
}
