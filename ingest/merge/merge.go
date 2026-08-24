// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

// Package merge overlays reference records onto the payload a transformation
// has already produced.
//
// A reference record is written in its own vocabulary and does not share the
// shape of the output — that is by design, since the same record may have to
// reach targets that agree on nothing structurally. So the overlay is two
// steps: relocate values from the record's vocabulary into the target's shape,
// then combine them with what is already there.
//
// The relocation is a table of src→dst paths, declared as Go where the
// transformer wires up its reference tables. It is deliberately constrained so
// it cannot become code: no expressions, no conditionals, no cross-field
// references, no computed values, array indices only as literals.
//
// The line this package holds is that a policy says how two *values* combine.
// It is not a language in which a publisher authors operations on the target —
// no "add this element", no "remove that one". Records carry data; the table
// carries meaning. Anything that needs more belongs in the transformer.
//
// Where a record comes from is not this package's concern. The one thing that
// must hold is that its shape is the one the table was written against, which
// is asserted by schema version rather than by inspecting the data — see
// Map.Schema.
package merge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Policy says how a record's value combines with what is already at the
// destination.
//
// Two values, and there is no second axis. A request for a third is the signal
// that the case belongs in the transformer rather than in a table.
type Policy string

const (
	// Merge combines objects key by key, per RFC 7386: keys the record does not
	// name survive. Arrays and scalars are replaced, because 7386 defines no
	// element semantics and inventing some here would make the table a language.
	//
	// The zero value, so a rule that says nothing gets it.
	Merge Policy = "merge"

	// Replace makes the record's value the destination outright, objects
	// included. Use it where one source owns a subtree and anything else
	// already there is wrong rather than merely older.
	Replace Policy = "replace"
)

// Rule relocates one value from the record's vocabulary into the target shape.
type Rule struct {
	Src    string
	Dst    string
	Policy Policy
}

// Map is the rule table for one (record vocabulary, target) pair.
type Map struct {
	// Target names what this maps into, for diagnostics only.
	Target string

	// Schema is the record vocabulary this table was written against, e.g.
	// "parking.v1". It is an assertion, not a validator: the publisher stamps
	// the version on the record and a mismatch means the table no longer
	// describes the data, whatever the data happens to look like. Hand it to
	// the reference table so records carrying another version are refused
	// before they ever reach a rule.
	Schema string

	Rules []Rule
}

type compiledRule struct {
	src    Path
	dst    Path
	policy Policy
	raw    Rule
}

// Compiled is a validated Map, ready to apply.
type Compiled struct {
	target string
	schema string
	rules  []compiledRule
}

func (c *Compiled) Target() string { return c.target }

// Schema is the record vocabulary this table expects. Pass it to the reference
// table's configuration so the version is declared in exactly one place.
func (c *Compiled) Schema() string { return c.schema }

// Compile validates a Map and prepares it.
//
// Validation is strict and happens once, at startup: a rule table is the
// highest-blast-radius data in a transformer, and a mistake in one should stop
// the process rather than silently drop a field at three in the morning.
func Compile(m Map) (*Compiled, error) {
	if len(m.Rules) == 0 {
		return nil, fmt.Errorf("map for %q has no rules", m.Target)
	}
	c := &Compiled{target: m.Target, schema: m.Schema}

	seenDst := map[string]Rule{}
	for i, r := range m.Rules {
		src, err := ParsePath(r.Src)
		if err != nil {
			return nil, fmt.Errorf("rule %d: src: %w", i, err)
		}
		dst, err := ParsePath(r.Dst)
		if err != nil {
			return nil, fmt.Errorf("rule %d: dst: %w", i, err)
		}

		policy := r.Policy
		if policy == "" {
			policy = Merge
		}
		switch policy {
		case Merge, Replace:
		default:
			return nil, fmt.Errorf("rule %d: unknown policy %q; the two are %s and %s",
				i, r.Policy, Merge, Replace)
		}

		// One destination may have only one source. Two rules writing the same
		// place is always a mistake, and the order they happen to be listed in
		// should never be what decides the outcome.
		key := dst.String()
		if prev, dup := seenDst[key]; dup {
			return nil, fmt.Errorf("rule %d: %s is already written by src %s; a destination may have only one source",
				i, key, prev.Src)
		}
		seenDst[key] = r

		c.rules = append(c.rules, compiledRule{src: src, dst: dst, policy: policy, raw: r})
	}
	return c, nil
}

// MustCompile is Compile for a table declared as a package-level variable.
//
// It panics, which is the point: a rule table is program structure, so a
// mistake in one should stop the process at load rather than surface as a
// missing field under load.
func MustCompile(m Map) *Compiled {
	c, err := Compile(m)
	if err != nil {
		panic(fmt.Sprintf("merge: invalid rule table for %q: %v", m.Target, err))
	}
	return c
}

// Apply overlays record onto target, in place.
//
// Both are decoded JSON documents. Because destinations are unique, the order
// rules are listed in does not affect the result.
//
// A src the record does not carry is skipped — a record is a partial overlay,
// and absence means "not supplied", never "clear this". A src the record
// carries as null is the way to say "clear this".
func (c *Compiled) Apply(target, record map[string]any) error {
	for _, r := range c.rules {
		value, present := r.src.Get(record)
		if !present {
			continue
		}

		// Null removes the destination, at any depth. Without this the same
		// literal means "delete" inside a merged object and "write JSON null"
		// at a rule's own destination — one function, two answers, decided by
		// nothing but how deep the value happened to sit.
		if value == nil {
			r.dst.Delete(target)
			continue
		}

		if r.policy == Replace {
			if err := r.dst.Set(target, value); err != nil {
				return fmt.Errorf("rule %s -> %s: %w", r.raw.Src, r.raw.Dst, err)
			}
			continue
		}

		// RFC 7386 at the destination: two objects merge recursively, so a
		// subtree rule leaves keys the transformation wrote under it alone
		// unless the record names them. Anything else replaces.
		existing, _ := r.dst.Get(target)
		if err := r.dst.Set(target, MergePatch(existing, value)); err != nil {
			return fmt.Errorf("rule %s -> %s: %w", r.raw.Src, r.raw.Dst, err)
		}
	}
	return nil
}

// Source is one reference record together with the table that relocates it.
type Source struct {
	// Name identifies the source in diagnostics.
	Name string
	Map  *Compiled
	// Record is the entity's reference record, or nil when this source has
	// nothing to say about it — in which case earlier layers stand.
	Record map[string]any
}

// ApplyAll overlays several sources onto one target, in the order given.
//
// Precedence is the order of the slice and nothing else: a later source
// overwrites an earlier one where they collide. There are no priority numbers,
// because someone always needs one that sits between two existing ones, and no
// recency comparison, because that would make the result depend on the order
// records happened to arrive rather than being a pure function of
// (target, records, rules).
func ApplyAll(target map[string]any, sources []Source) error {
	for _, s := range sources {
		if s.Map == nil || s.Record == nil {
			continue
		}
		if err := s.Map.Apply(target, s.Record); err != nil {
			return fmt.Errorf("source %q: %w", s.Name, err)
		}
	}
	return nil
}

// decode reads a document without converting its numbers.
//
// The default decoder turns every JSON number into a float64, which has a
// 53-bit mantissa — enough to silently round a large integer that happens to be
// sitting in an untyped map. UseNumber keeps the literal exactly as written, so
// the round trip below is lossless: what a field held going in is what it holds
// coming out, whatever its magnitude.
func decode(b []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(into)
}

// Into applies fn to a typed value by round-tripping it through its JSON form.
//
// The merge works on decoded JSON, so a typed target has to be marshalled,
// patched and unmarshalled back. The consequence is worth stating plainly: the
// rules address the *serialized* shape, so a field with no json tag is
// invisible to them, and a field the target type does not declare is dropped on
// the way back.
func Into(target any, fn func(map[string]any) error) error {
	b, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("target is not serializable: %w", err)
	}
	var doc map[string]any
	if err := decode(b, &doc); err != nil {
		return fmt.Errorf("target does not decode to an object: %w", err)
	}
	if err := fn(doc); err != nil {
		return err
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("merged document is not serializable: %w", err)
	}
	if err := decode(out, target); err != nil {
		return fmt.Errorf("merged document does not fit the target type: %w", err)
	}
	return nil
}

// Describe renders the table for startup logs.
func (c *Compiled) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (schema %s)\n", c.target, c.schema)
	for _, r := range c.rules {
		fmt.Fprintf(&b, "  %-28s -> %-34s %s\n", r.src, r.dst, r.policy)
	}
	return b.String()
}
