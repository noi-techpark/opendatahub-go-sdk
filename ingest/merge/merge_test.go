// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package merge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("fixture is not json: %v", err)
	}
	return m
}

// parkingToStation is the shape a transformer declares at wire-up time.
var parkingToStation = MustCompile(Map{
	Target: "bdp.Station",
	Schema: "parking.v1",
	Rules: []Rule{
		{Src: "$.names.it", Dst: "$.name"},
		{Src: "$.gps.lat", Dst: "$.latitude"},
		{Src: "$.gps.lon", Dst: "$.longitude"},
		{Src: "$.names.de", Dst: "$.metaData.name_de"},
		{Src: "$.names.it", Dst: "$.metaData.name_it"},
		{Src: "$.municipality", Dst: "$.metaData.municipality"},
		// The record owns the whole netex subtree; anything else there is wrong
		// rather than merely older.
		{Src: "$.netex", Dst: "$.metaData.netex_parking", Policy: Replace},
	},
})

// ---------------------------------------------------------------- paths

func TestParsePath(t *testing.T) {
	ok := map[string]string{
		"$":                     "$",
		"$.name":                "$.name",
		"$.gps.lat":             "$.gps.lat",
		"$.GpsInfo[0].Latitude": "$.GpsInfo[0].Latitude",
		"$.metaData.netex":      "$.metaData.netex",
	}
	for in, want := range ok {
		p, err := ParsePath(in)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("ParsePath(%q).String() = %q", in, p.String())
		}
	}

	// Everything that would turn a rule into an expression is refused.
	for _, bad := range []string{"", "name", "$.", "$..a", "$.a[", "$.a[x]", "$.a[-1]", "$.a[*]", "$.a?(@.b)"} {
		if _, err := ParsePath(bad); err == nil {
			t.Errorf("ParsePath(%q) was accepted", bad)
		}
	}
}

func TestSetCreatesIntermediates(t *testing.T) {
	root := map[string]any{}
	p, _ := ParsePath("$.a.b.c")
	if err := p.Set(root, 1.0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := p.Get(root)
	if !ok || got != 1.0 {
		t.Errorf("Get after Set = %v, %v", got, ok)
	}
}

func TestSetGrowsFixedArrayIndex(t *testing.T) {
	root := map[string]any{}
	p, _ := ParsePath("$.GpsInfo[0].Latitude")
	if err := p.Set(root, 46.4); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok := p.Get(root)
	if !ok || v != 46.4 {
		t.Fatalf("Get = %v, %v", v, ok)
	}
	arr, ok := root["GpsInfo"].([]any)
	if !ok || len(arr) != 1 {
		t.Errorf("GpsInfo = %#v, want a one-element array", root["GpsInfo"])
	}
}

// ---------------------------------------------------------- merge patch

// The property that makes a subtree rule safe under Merge: keys the
// transformation wrote under the destination survive unless the record names
// them.
func TestMergePatchKeepsUnnamedKeys(t *testing.T) {
	target := doc(t, `{"provider_id":"0607242_0","capacity":245}`)
	patch := doc(t, `{"name_it":"NOI Techpark"}`)

	got := MergePatch(target, patch).(map[string]any)
	for _, k := range []string{"provider_id", "capacity", "name_it"} {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q was lost by a subtree merge", k)
		}
	}
	if got["capacity"] != 245.0 {
		t.Errorf("capacity = %v, want the transformation's 245", got["capacity"])
	}
}

func TestMergePatchNullRemoves(t *testing.T) {
	target := doc(t, `{"name_it":"Wrong","municipality":"Bolzano"}`)
	got := MergePatch(target, doc(t, `{"name_it":null}`)).(map[string]any)
	if _, ok := got["name_it"]; ok {
		t.Error("a null member did not remove the key")
	}
	if got["municipality"] != "Bolzano" {
		t.Error("an unrelated key was disturbed")
	}
}

func TestMergePatchReplacesArraysWholesale(t *testing.T) {
	target := doc(t, `{"a":[1,2,3]}`)
	got := MergePatch(target, doc(t, `{"a":[9]}`)).(map[string]any)
	if !reflect.DeepEqual(got["a"], []any{9.0}) {
		t.Errorf("a = %v, want the patch's array wholesale", got["a"])
	}
}

// ---------------------------------------------------------- compilation

func TestCompileRejectsTwoSourcesForOneDestination(t *testing.T) {
	_, err := Compile(Map{Target: "t", Rules: []Rule{
		{Src: "$.names.it", Dst: "$.name"},
		{Src: "$.standard_name", Dst: "$.name"},
	}})
	if err == nil {
		t.Fatal("two rules writing one destination were accepted")
	}
}

func TestCompileRejectsUnknownPolicy(t *testing.T) {
	_, err := Compile(Map{Target: "t", Rules: []Rule{
		{Src: "$.a", Dst: "$.b", Policy: "union"},
	}})
	if err == nil {
		t.Fatal("a third policy was accepted; that case belongs in the transformer")
	}
}

func TestCompileRejectsBadPathsAndEmptyTables(t *testing.T) {
	if _, err := Compile(Map{Target: "t", Rules: []Rule{{Src: "a", Dst: "$.b"}}}); err == nil {
		t.Error("a src without $ was accepted")
	}
	if _, err := Compile(Map{Target: "t"}); err == nil {
		t.Error("an empty table was accepted")
	}
}

func TestMustCompilePanicsOnABadTable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustCompile accepted a table with two sources for one destination")
		}
	}()
	MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.a", Dst: "$.x"},
		{Src: "$.b", Dst: "$.x"},
	}})
}

// The schema version is declared once, on the table, so the reference table can
// be configured from it rather than repeating the string.
func TestSchemaIsReadableForWiringTheReferenceTable(t *testing.T) {
	if parkingToStation.Schema() != "parking.v1" {
		t.Errorf("Schema() = %q", parkingToStation.Schema())
	}
	if parkingToStation.Target() != "bdp.Station" {
		t.Errorf("Target() = %q", parkingToStation.Target())
	}
	if parkingToStation.Describe() == "" {
		t.Error("Describe returned nothing")
	}
}

// ---------------------------------------------------------------- apply

func TestApplyRelocatesIntoTargetShape(t *testing.T) {
	// what the transformation produced
	target := doc(t, `{
	  "name": "0607242_0",
	  "latitude": 0, "longitude": 0,
	  "metaData": {"provider_id":"0607242_0","capacity":245,"carpark_id":0}
	}`)
	// what the record carries, in its own vocabulary
	record := doc(t, `{
	  "names": {"it":"Parcheggio NOI Techpark","de":"Parkplatz NOI Techpark"},
	  "municipality": "Bolzano - Bozen",
	  "gps": {"lat":46.4,"lon":11.34},
	  "netex": {"charging":true,"layout":"multistorey"}
	}`)

	if err := parkingToStation.Apply(target, record); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if target["name"] != "Parcheggio NOI Techpark" {
		t.Errorf("name = %v", target["name"])
	}
	if target["latitude"] != 46.4 {
		t.Errorf("latitude = %v", target["latitude"])
	}

	meta := target["metaData"].(map[string]any)
	if meta["name_de"] != "Parkplatz NOI Techpark" || meta["municipality"] != "Bolzano - Bozen" {
		t.Errorf("record metadata missing: %v", meta)
	}
	// what the transformation wrote survived, which is the whole point
	if meta["capacity"] != 245.0 || meta["provider_id"] != "0607242_0" {
		t.Errorf("transformation metadata was clobbered: %v", meta)
	}
	// the netex subtree landed nested, with real booleans
	netex := meta["netex_parking"].(map[string]any)
	if netex["charging"] != true || netex["layout"] != "multistorey" {
		t.Errorf("netex = %v", netex)
	}
}

// A record is a partial overlay: what it does not carry, it does not touch.
func TestApplyLeavesUnsuppliedFieldsAlone(t *testing.T) {
	target := doc(t, `{"name":"Provider Name","latitude":11.0,"metaData":{"capacity":245}}`)

	if err := parkingToStation.Apply(target, doc(t, `{"municipality":"Merano"}`)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if target["name"] != "Provider Name" {
		t.Errorf("name was changed by a record that does not carry one: %v", target["name"])
	}
	if target["latitude"] != 11.0 {
		t.Errorf("latitude was changed: %v", target["latitude"])
	}
}

// --------------------------------------------------------------- policies

// Merge is per key: the destination keeps what the record does not name. This
// is the right default and the wrong answer when a source owns a subtree.
func TestMergeKeepsWhatTheRecordDoesNotName(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.netex", Dst: "$.metaData.netex_parking"},
	}})
	target := doc(t, `{"metaData":{"netex_parking":{"layout":"covered","charging":false}}}`)

	if err := c.Apply(target, doc(t, `{"netex":{"charging":true}}`)); err != nil {
		t.Fatal(err)
	}
	netex := target["metaData"].(map[string]any)["netex_parking"].(map[string]any)
	if netex["charging"] != true {
		t.Errorf("charging = %v, want the record's true", netex["charging"])
	}
	if netex["layout"] != "covered" {
		t.Errorf("layout = %v, want the untouched key to survive a merge", netex["layout"])
	}
}

// Replace is the gap this closes: before it, a source could not say "this
// subtree is mine" — merge always recursed and left foreign keys behind.
func TestReplaceOwnsAnObjectWholesale(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.netex", Dst: "$.metaData.netex_parking", Policy: Replace},
	}})
	target := doc(t, `{"metaData":{"netex_parking":{"layout":"covered","charging":false},"capacity":245}}`)

	if err := c.Apply(target, doc(t, `{"netex":{"charging":true}}`)); err != nil {
		t.Fatal(err)
	}
	meta := target["metaData"].(map[string]any)
	netex := meta["netex_parking"].(map[string]any)
	if len(netex) != 1 || netex["charging"] != true {
		t.Errorf("netex_parking = %v, want only the record's own keys", netex)
	}
	// Replace owns its destination, not its neighbours.
	if meta["capacity"] != 245.0 {
		t.Errorf("a sibling key was destroyed by a replace: %v", meta)
	}
}

func TestReplaceAlsoOwnsScalarsAndArrays(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.tags", Dst: "$.metaData.tags", Policy: Replace},
		{Src: "$.name", Dst: "$.name", Policy: Replace},
	}})
	target := doc(t, `{"name":"provider","metaData":{"tags":["covered","lit"]}}`)

	if err := c.Apply(target, doc(t, `{"name":"record","tags":["ev"]}`)); err != nil {
		t.Fatal(err)
	}
	if target["name"] != "record" {
		t.Errorf("name = %v", target["name"])
	}
	got := target["metaData"].(map[string]any)["tags"]
	if !reflect.DeepEqual(got, []any{"ev"}) {
		t.Errorf("tags = %v, want the record's array wholesale", got)
	}
}

// ------------------------------------------------------------------ null

// Null removes the destination. Before this it wrote JSON null at a rule's own
// destination while deleting one level deeper — the same literal meaning two
// different things depending on depth.
func TestNullRemovesTheDestination(t *testing.T) {
	target := doc(t, `{"metaData":{"name_de":"Parkplatz","capacity":245}}`)

	if err := parkingToStation.Apply(target, doc(t, `{"names":{"de":null}}`)); err != nil {
		t.Fatal(err)
	}
	meta := target["metaData"].(map[string]any)
	if v, ok := meta["name_de"]; ok {
		t.Errorf("name_de = %v (present), want the key removed", v)
	}
	if meta["capacity"] != 245.0 {
		t.Errorf("an unrelated key was disturbed: %v", meta)
	}
}

func TestNullRemovesUnderReplaceToo(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.netex", Dst: "$.metaData.netex_parking", Policy: Replace},
	}})
	target := doc(t, `{"metaData":{"netex_parking":{"charging":true}}}`)

	if err := c.Apply(target, doc(t, `{"netex":null}`)); err != nil {
		t.Fatal(err)
	}
	if v, ok := target["metaData"].(map[string]any)["netex_parking"]; ok {
		t.Errorf("netex_parking = %v (present), want it removed", v)
	}
}

// Deleting one key inside a merged object still works, and now means the same
// thing as a null at a destination.
func TestNullInsideAMergedObjectRemovesThatKey(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{
		{Src: "$.netex", Dst: "$.metaData.netex_parking"},
	}})
	target := doc(t, `{"metaData":{"netex_parking":{"layout":"covered","charging":true}}}`)

	if err := c.Apply(target, doc(t, `{"netex":{"charging":null}}`)); err != nil {
		t.Fatal(err)
	}
	netex := target["metaData"].(map[string]any)["netex_parking"].(map[string]any)
	if _, ok := netex["charging"]; ok {
		t.Error("charging was not removed")
	}
	if netex["layout"] != "covered" {
		t.Errorf("layout = %v, want it untouched", netex["layout"])
	}
}

// Omitting a field and nulling it are different, and the difference only shows
// where something else already wrote the destination.
func TestOmittedIsNotTheSameAsNull(t *testing.T) {
	target := doc(t, `{"name":"Provider Name"}`)
	if err := parkingToStation.Apply(target, doc(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	if target["name"] != "Provider Name" {
		t.Errorf("an omitted field disturbed the destination: %v", target["name"])
	}

	if err := parkingToStation.Apply(target, doc(t, `{"names":{"it":null}}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := target["name"]; ok {
		t.Error("a null did not remove the destination")
	}
}

// ------------------------------------------------------------ precedence

func TestApplyAllPrecedenceIsDeclaredOrder(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{{Src: "$.name", Dst: "$.name"}}})
	target := doc(t, `{"name":"provider"}`)

	if err := ApplyAll(target, []Source{
		{Name: "first", Map: c, Record: doc(t, `{"name":"first"}`)},
		{Name: "second", Map: c, Record: doc(t, `{"name":"second"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if target["name"] != "second" {
		t.Errorf("name = %v, want the later source to win", target["name"])
	}
}

// A source with nothing to say about an entity leaves earlier layers standing.
func TestApplyAllSkipsSourcesWithNoRecord(t *testing.T) {
	c := MustCompile(Map{Target: "t", Rules: []Rule{{Src: "$.name", Dst: "$.name"}}})
	target := doc(t, `{"name":"provider"}`)

	if err := ApplyAll(target, []Source{
		{Name: "first", Map: c, Record: doc(t, `{"name":"first"}`)},
		{Name: "second", Map: c, Record: nil},
	}); err != nil {
		t.Fatal(err)
	}
	if target["name"] != "first" {
		t.Errorf("name = %v; an absent record must not clear an earlier layer", target["name"])
	}
}

// ------------------------------------------------------------- typed IO

type station struct {
	Name      string         `json:"name"`
	Latitude  float64        `json:"latitude"`
	Longitude float64        `json:"longitude"`
	MetaData  map[string]any `json:"metaData"`
	secret    string         // unexported: invisible to the rules, must survive
}

func TestIntoRoundTripsATypedTarget(t *testing.T) {
	s := &station{
		Name:     "0607242_0",
		MetaData: map[string]any{"provider_id": "0607242_0", "capacity": 245},
		secret:   "kept",
	}

	err := Into(s, func(d map[string]any) error {
		return parkingToStation.Apply(d, doc(t, `{"names":{"it":"NOI"},"gps":{"lat":46.4,"lon":11.34}}`))
	})
	if err != nil {
		t.Fatalf("Into: %v", err)
	}

	if s.Name != "NOI" || s.Latitude != 46.4 || s.Longitude != 11.34 {
		t.Errorf("station = %+v", s)
	}
	// Numbers in an untyped map come back as json.Number, holding the original
	// literal rather than a float64 approximation of it.
	if got := fmt.Sprint(s.MetaData["capacity"]); got != "245" {
		t.Errorf("capacity = %v (%T), want the literal 245", s.MetaData["capacity"], s.MetaData["capacity"])
	}
	if s.MetaData["provider_id"] != "0607242_0" {
		t.Errorf("transformation metadata lost in the round trip: %v", s.MetaData)
	}
	if s.secret != "kept" {
		t.Errorf("an unexported field was disturbed: %q", s.secret)
	}
}

// The round trip must not quietly change a number. A float64 has a 53-bit
// mantissa, so an integer past 2^53 comes back a different integer — which is
// the kind of corruption nobody notices until an id stops matching.
func TestIntoPreservesNumbersExactly(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1, not representable as a float64
	s := &station{MetaData: map[string]any{
		"big":     json.RawMessage(big),
		"decimal": json.RawMessage("46.71602"),
		"small":   json.RawMessage("245"),
	}}

	if err := Into(s, func(d map[string]any) error { return nil }); err != nil {
		t.Fatalf("Into: %v", err)
	}

	for key, want := range map[string]string{
		"big": big, "decimal": "46.71602", "small": "245",
	} {
		if got := fmt.Sprint(s.MetaData[key]); got != want {
			t.Errorf("%s = %s, want %s — the round trip changed the value", key, got, want)
		}
	}

	out, err := json.Marshal(s.MetaData)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"big":`+big) {
		t.Errorf("marshalled as %s, want big to stay a bare number", out)
	}
}
